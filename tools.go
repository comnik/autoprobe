package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DefaultBashTimeoutMillis bounds every bash tool call. Without it, a single
// agent-issued command that doesn't return (ncurses programs, `tail -f`, a
// stuck network call) would freeze the whole eval loop.
const DefaultBashTimeoutMillis = 60_000

// DefaultBashMaxOutputBytes caps the in-memory output buffered for a single
// bash call. Past this, runBash spills the full output to a temp file and
// returns only the tail (with a marker pointing at the file). 64KB is small
// enough that a runaway `cat huge.log` or `find /` doesn't blow the agent's
// context window, and large enough to cover typical command output.
const DefaultBashMaxOutputBytes = 64 * 1024

type ToolDefinition struct {
	Name        string
	Description string
	Parameters  map[string]any // JSON Schema; each provider translates to its native shape
	Function    func(input json.RawMessage) (string, error)
}

var DefaultTools = []ToolDefinition{ReadTool, BashTool, EditTool, WriteTool}

var ReadTool = ToolDefinition{
	Name:        "read",
	Description: "Read the contents of a file at the given path. Returns the file's text contents.",
	Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to the file to read.",
			},
		},
		"required": []string{"path"},
	},
	Function: readFile,
}

func readFile(input json.RawMessage) (string, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	data, err := os.ReadFile(args.Path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

var BashTool = ToolDefinition{
	Name:        "bash",
	Description: "Execute a bash command and return the combined stdout and stderr.",
	Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "Bash command to execute.",
			},
			"timeout": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf(
					"Optional timeout in milliseconds (must be > 0). Defaults to %d. "+
						"On expiry the whole process group is killed.",
					DefaultBashTimeoutMillis,
				),
				"minimum": 1,
			},
		},
		"required": []string{"command"},
	},
	Function: runBash,
}

// BashOperations executes a bash command. Carving this out of runBash lets
// tests inject a deterministic fake and lets future backends (SSH, sandbox,
// container exec) plug in without touching the tool wrapper. Mirrors the
// shape of pi-mono's BashOperations.
type BashOperations interface {
	// Exec runs command, streaming output through onData as it arrives.
	// exitCode is meaningful only when err == nil. err covers spawn/IO
	// failures and context cancellation (timeout or caller-driven).
	Exec(ctx context.Context, command string, onData func([]byte)) (exitCode int, err error)
}

// ExitError is returned by runBash when the command ran to completion with
// a non-zero exit. Distinct from spawn errors, timeouts, or cancellation.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }
func (e *ExitError) ExitCode() int { return e.Code }

// defaultBashOps is the production executor. Tests inject their own via
// runBashWith.
var defaultBashOps BashOperations = localBashOps{}

type localBashOps struct{}

func (localBashOps) Exec(ctx context.Context, command string, onData func([]byte)) (int, error) {
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	// Put bash and its descendants in a fresh process group so we can kill the
	// whole tree on timeout. Otherwise SIGKILL hits bash but leaves ncurses
	// children (e.g. cmatrix) running, draining the eval host.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// Backstop: if Cancel returns but some descendant is still holding stdio
	// pipes open, Wait would otherwise hang indefinitely.
	cmd.WaitDelay = 5 * time.Second

	w := &onDataWriter{fn: onData}
	cmd.Stdout = w
	cmd.Stderr = w

	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}

// onDataWriter adapts an exec.Cmd Stdout/Stderr writer to the BashOperations
// onData callback. exec.Cmd spawns separate goroutines for stdout and stderr
// when the writers differ, but uses a single writer when Stdout == Stderr;
// even so, we serialize for safety since onData may be wired to a shared
// buffer.
type onDataWriter struct {
	fn func([]byte)
	mu sync.Mutex
}

func (w *onDataWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fn != nil {
		w.fn(p)
	}
	return len(p), nil
}

func runBash(input json.RawMessage) (string, error) {
	return runBashWith(defaultBashOps, input)
}

func runBashWith(ops BashOperations, input json.RawMessage) (string, error) {
	var args struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout,omitempty"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	timeoutMs := args.Timeout
	if timeoutMs <= 0 {
		timeoutMs = DefaultBashTimeoutMillis
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	acc := newOutputAccumulator(DefaultBashMaxOutputBytes)
	defer acc.Close()
	onData := func(data []byte) { acc.Append(data) }

	exitCode, err := ops.Exec(ctx, args.Command, onData)
	out := acc.Render()

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return out, fmt.Errorf("timed out after %dms (process group killed)", timeoutMs)
	}
	if err != nil {
		return out, fmt.Errorf("%s: %w", strings.TrimSpace(out), err)
	}
	if exitCode != 0 {
		return out, fmt.Errorf("%s: %w", strings.TrimSpace(out), &ExitError{Code: exitCode})
	}
	return out, nil
}

// outputAccumulator buffers bash output with two safety nets:
//   - rolling window keeps at most ~2*maxBytes in memory
//   - once total received exceeds maxBytes, full output also spills to a
//     temp file so the agent can still get at it via a marker in the result
//
// Render returns the tail of the buffered output (aligned to a line
// boundary), with a truncation marker appended when applicable.
type outputAccumulator struct {
	mu sync.Mutex

	maxBytes int

	chunks        [][]byte
	bufferedBytes int
	totalBytes    int

	tempFile *os.File
	tempPath string
}

func newOutputAccumulator(maxBytes int) *outputAccumulator {
	return &outputAccumulator{maxBytes: maxBytes}
}

func (a *outputAccumulator) Append(data []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.totalBytes += len(data)

	// Once we cross the threshold, spill everything to a temp file. The
	// rolling window can only show the tail; the spill preserves the head
	// for anyone willing to read the file.
	if a.totalBytes > a.maxBytes && a.tempFile == nil {
		if f, err := os.CreateTemp("", "autoprobe-bash-*.log"); err == nil {
			a.tempFile = f
			a.tempPath = f.Name()
			for _, c := range a.chunks {
				_, _ = f.Write(c)
			}
		}
	}
	if a.tempFile != nil {
		_, _ = a.tempFile.Write(data)
	}

	cp := make([]byte, len(data))
	copy(cp, data)
	a.chunks = append(a.chunks, cp)
	a.bufferedBytes += len(cp)
	for a.bufferedBytes > 2*a.maxBytes && len(a.chunks) > 1 {
		a.bufferedBytes -= len(a.chunks[0])
		a.chunks = a.chunks[1:]
	}
}

func (a *outputAccumulator) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tempFile != nil {
		_ = a.tempFile.Close()
		a.tempFile = nil
	}
}

func (a *outputAccumulator) Render() string {
	a.mu.Lock()
	defer a.mu.Unlock()

	full := bytes.Join(a.chunks, nil)
	if a.totalBytes <= a.maxBytes {
		return string(full)
	}

	tail := full
	if len(tail) > a.maxBytes {
		tail = tail[len(tail)-a.maxBytes:]
	}
	// Align to a line boundary so the first visible line isn't a fragment.
	if i := bytes.IndexByte(tail, '\n'); i != -1 && i < len(tail)-1 {
		tail = tail[i+1:]
	}

	marker := fmt.Sprintf(
		"\n\n[Output truncated: showing last %dB of %dB. Full output: %s]",
		len(tail), a.totalBytes, a.tempPath,
	)
	if a.tempPath == "" {
		marker = fmt.Sprintf(
			"\n\n[Output truncated: showing last %dB of %dB. (Failed to spill full output to temp file.)]",
			len(tail), a.totalBytes,
		)
	}
	return string(tail) + marker
}

var EditTool = ToolDefinition{
	Name:        "edit",
	Description: "Replace exactly one occurrence of old_string with new_string in the file at path. Fails if old_string is missing or appears more than once.",
	Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to the file to edit.",
			},
			"old_string": map[string]any{
				"type":        "string",
				"description": "Exact text to be replaced.",
			},
			"new_string": map[string]any{
				"type":        "string",
				"description": "Replacement text.",
			},
		},
		"required": []string{"path", "old_string", "new_string"},
	},
	Function: editFile,
}

func editFile(input json.RawMessage) (string, error) {
	var args struct {
		Path      string `json:"path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	data, err := os.ReadFile(args.Path)
	if err != nil {
		return "", err
	}
	content := string(data)
	count := strings.Count(content, args.OldString)
	if count == 0 {
		return "", fmt.Errorf("old_string not found in %s", args.Path)
	}
	if count > 1 {
		return "", fmt.Errorf("old_string appears %d times in %s; must be unique", count, args.Path)
	}
	updated := strings.Replace(content, args.OldString, args.NewString, 1)
	if err := os.WriteFile(args.Path, []byte(updated), 0644); err != nil {
		return "", err
	}
	return "ok", nil
}

var WriteTool = ToolDefinition{
	Name:        "write",
	Description: "Create a new file or overwrite an existing file with the given content.",
	Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to the file to write.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "Content to write to the file.",
			},
		},
		"required": []string{"path", "content"},
	},
	Function: writeFile,
}

func writeFile(input json.RawMessage) (string, error) {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	if err := os.WriteFile(args.Path, []byte(args.Content), 0644); err != nil {
		return "", err
	}
	return "ok", nil
}
