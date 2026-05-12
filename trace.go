package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/comnik/autoprobe/internal/provider"
)

const (
	// traceDirName is the well-known, single-slot directory autoprobe writes
	// the current run's trace into. The lifecycle is "clear and recreate at
	// the start of every run" — operators who want to keep a particular
	// trace move it aside before launching the next run.
	traceDirName = ".autoprobe-last-run"

	// traceLogFileName is the append-only JSONL index. Line 1 is a `run`
	// header; every subsequent line is an `iteration` summary written after
	// that iteration finishes.
	traceLogFileName = "log.jsonl"

	// traceFormatVersion is stamped into both the run header and every
	// per-iteration record. Bumped only when the on-disk shape changes in a
	// way the viewer would need to branch on.
	traceFormatVersion = 1

	// tracePadding is the zero-pad width for iteration file names. Runs that
	// exceed this still produce valid filenames; they just sort less neatly
	// past the cliff.
	tracePadding = 5
)

// Tracer captures one autoprobe run's iteration-by-iteration record into
// traceDirName. Tracing is unconditional and best-effort: a failed write
// surfaces as a warning to stderr and never aborts the run.
//
// The mutex guards the log file handle; in current usage the Tracer is
// driven serially from Agent.Step but the lock keeps it safe if the call
// site ever fans out.
type Tracer struct {
	dir string
	log *os.File
	mu  sync.Mutex
}

// NewTracer clears and recreates dir, then opens log.jsonl for appending.
// If removal fails (e.g. a file in the directory is held open by another
// process), the caller should abort the run rather than mix two runs'
// records together — the design rules out best-effort cleanup here.
func NewTracer(dir string) (*Tracer, error) {
	if err := os.RemoveAll(dir); err != nil {
		return nil, fmt.Errorf("preparing trace dir %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating trace dir %s: %w", dir, err)
	}
	f, err := os.OpenFile(filepath.Join(dir, traceLogFileName), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening trace log: %w", err)
	}
	return &Tracer{dir: dir, log: f}, nil
}

// Close releases the log file. Safe to call multiple times.
func (t *Tracer) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.log == nil {
		return nil
	}
	err := t.log.Close()
	t.log = nil
	return err
}

// RunHeader is the first line of log.jsonl. Carries the run-level metadata
// the viewer needs to label the trace; per-iteration records intentionally
// do not duplicate any of it.
type RunHeader struct {
	Kind                string    `json:"kind"`
	FormatVersion       int       `json:"format_version"`
	AutoprobeVersion    string    `json:"autoprobe_version"`
	StartedAt           time.Time `json:"started_at"`
	ProbeDir            string    `json:"probe_dir"`
	Provider            string    `json:"provider"`
	Model               string    `json:"model"`
	Goal                string    `json:"goal,omitempty"`
	ContextBudgetTokens int       `json:"context_budget_tokens"`
}

// WriteRunHeader appends the run header line. Caller fills the data
// fields; Kind and FormatVersion are stamped here so callers can't
// accidentally drift from the on-disk format.
func (t *Tracer) WriteRunHeader(h RunHeader) error {
	if t == nil {
		return nil
	}
	h.Kind = "run"
	h.FormatVersion = traceFormatVersion
	return t.appendLogLine(h)
}

// iterationLogEntry is the JSON shape of a non-header line in log.jsonl.
// It's a summary — enough for the viewer's left rail without forcing the
// viewer to fetch the per-iteration file just to render the list.
type iterationLogEntry struct {
	Kind                string    `json:"kind"`
	N                   int       `json:"n"`
	File                string    `json:"file"`
	StartedAt           time.Time `json:"started_at"`
	DurationMs          int64     `json:"duration_ms"`
	StopReason          string    `json:"stop_reason"`
	Overflowed          bool      `json:"overflowed"`
	RevisionPromptFired bool      `json:"revision_prompt_fired"`
	IdlePollsBefore     int       `json:"idle_polls_before"`
	InputTokens         int       `json:"input_tokens"`
	OutputTokens        int       `json:"output_tokens"`
}

// IterationTrace is the on-disk shape of iter-NNNNN.json — the exhaustive
// per-iteration slice the viewer renders. Built by the agent at the tail
// of Step and handed to WriteIteration.
type IterationTrace struct {
	FormatVersion   int                      `json:"format_version"`
	Iteration       int                      `json:"iteration"`
	StartedAt       time.Time                `json:"started_at"`
	CompletedAt     time.Time                `json:"completed_at"`
	IdlePollsBefore int                      `json:"idle_polls_before"`
	IdleWaitMs      int64                    `json:"idle_wait_ms"`
	Context         TraceContext             `json:"context"`
	Response        TraceResponse            `json:"response"`
	ToolResults     []TraceToolResult        `json:"tool_results"`
	Programs        []TraceProgram           `json:"programs"`
	Budget          TraceBudget              `json:"budget"`
	StatsSnapshot   map[string]*programStats `json:"stats_snapshot"`
}

// TraceContext is the exact context window the harness sent to the
// provider for this iteration: a flat sequence of user / assistant /
// tool_result messages in the order they were submitted.
type TraceContext struct {
	Messages []traceMessage `json:"messages"`
}

// traceMessage is the union JSON shape across user, assistant, and
// tool_result roles. Fields that don't apply to a given role are omitted
// via `omitempty` rather than nested under a discriminator — the role
// already discriminates.
type traceMessage struct {
	Role       string           `json:"role"`
	Content    []map[string]any `json:"content,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolName   string           `json:"tool_name,omitempty"`
	IsError    bool             `json:"is_error,omitempty"`
}

// TraceResponse is the assistant message the provider returned for this
// iteration: the model id, stop reason, token usage, and the content
// blocks in their original order.
type TraceResponse struct {
	Model      string           `json:"model"`
	StopReason string           `json:"stop_reason"`
	Usage      TraceUsage       `json:"usage"`
	Content    []map[string]any `json:"content"`
}

// TraceUsage mirrors provider.Usage in JSON form. Fields the provider did
// not report are zero.
type TraceUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// TraceToolResult is one synthesized tool result, flattened to a single
// string of content text since the tool layer never produces multiple
// text blocks.
type TraceToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	ToolName   string `json:"tool_name"`
	Content    string `json:"content"`
	IsError    bool   `json:"is_error"`
}

// TraceProgram is the per-program slice of one iteration: everything the
// renderer needs to show a sortable table without re-parsing the
// conversation. `Included` is whether this program's full rendered output
// (not a sentinel, not a random-skip) reached the context.
// `ExplorationPhase` is set only for inactive programs and tags which
// channel they fed through ("nonzero" or "random"); empty for active.
type TraceProgram struct {
	Name             string `json:"name"`
	Exit             int    `json:"exit"`
	LatencyMs        int64  `json:"latency_ms"`
	Active           bool   `json:"active"`
	Included         bool   `json:"included"`
	ExplorationPhase string `json:"exploration_phase,omitempty"`
	Output           string `json:"output"`
	OutputTokens     int    `json:"output_tokens"`
}

// TraceBudget captures the budget bookkeeping for this iteration in a
// shape the viewer can render directly. `UsedTokens` is the pre-selection
// total (sum of every program's rendered token cost), matching what the
// overflow check evaluated.
type TraceBudget struct {
	LimitTokens             int  `json:"limit_tokens"`
	UsedTokens              int  `json:"used_tokens"`
	Overflowed              bool `json:"overflowed"`
	RevisionPromptFired     bool `json:"revision_prompt_fired"`
	ActiveBudgetTokens      int  `json:"active_budget_tokens"`
	ExplorationBudgetTokens int  `json:"exploration_budget_tokens"`
}

// WriteIteration writes the per-iteration JSON file with tmp+rename so a
// process killed mid-write never leaves a partially-written iter file,
// then appends the matching summary line to log.jsonl. The order is
// intentional: the log must never reference a file that isn't on disk
// yet. Returns the first error encountered; on iter-file failure no log
// line is appended.
func (t *Tracer) WriteIteration(rec IterationTrace) error {
	if t == nil {
		return nil
	}
	rec.FormatVersion = traceFormatVersion

	file := iterationFileName(rec.Iteration)
	path := filepath.Join(t.dir, file)
	if err := writeJSONAtomic(path, &rec); err != nil {
		return err
	}

	entry := iterationLogEntry{
		Kind:                "iteration",
		N:                   rec.Iteration,
		File:                file,
		StartedAt:           rec.StartedAt,
		DurationMs:          rec.CompletedAt.Sub(rec.StartedAt).Milliseconds(),
		StopReason:          rec.Response.StopReason,
		Overflowed:          rec.Budget.Overflowed,
		RevisionPromptFired: rec.Budget.RevisionPromptFired,
		IdlePollsBefore:     rec.IdlePollsBefore,
		InputTokens:         rec.Response.Usage.InputTokens,
		OutputTokens:        rec.Response.Usage.OutputTokens,
	}
	return t.appendLogLine(entry)
}

// appendLogLine marshals v as compact JSON and writes a single
// newline-terminated record to log.jsonl. POSIX `O_APPEND` guarantees
// the append is atomic with respect to concurrent readers, so a viewer
// or `tail -f` never sees a partial line.
func (t *Tracer) appendLogLine(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.log == nil {
		return fmt.Errorf("trace log closed")
	}
	_, err = t.log.Write(data)
	return err
}

func iterationFileName(n int) string {
	return fmt.Sprintf("iter-%0*d.json", tracePadding, n)
}

// writeJSONAtomic writes v as indented JSON to path via a sibling tmp
// file plus rename. The `.tmp` lives next to the target on the same
// filesystem so rename is atomic; on failure the tmp is left behind for
// inspection rather than silently cleaned up.
func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// stopReasonString maps provider.StopReason to its trace-format string.
// Kept here, not on the provider package, because the strings are part
// of the trace's on-disk contract and shouldn't get rewired by changes
// in the provider layer.
func stopReasonString(s provider.StopReason) string {
	switch s {
	case provider.StopEnd:
		return "end"
	case provider.StopMaxTokens:
		return "max_tokens"
	case provider.StopToolUse:
		return "tool_use"
	case provider.StopError:
		return "error"
	default:
		return "unknown"
	}
}

// serializeContextMessages turns the live conversation slice into the
// JSON shape the design specifies for context.messages. Provider-native
// signature fields are preserved verbatim; the viewer hides them by
// default but the bytes are there for debugging continuity tokens.
func serializeContextMessages(msgs []provider.Message) []traceMessage {
	out := make([]traceMessage, 0, len(msgs))
	for _, m := range msgs {
		switch m := m.(type) {
		case provider.UserMessage:
			content := make([]map[string]any, 0, len(m.Content))
			for _, c := range m.Content {
				content = append(content, map[string]any{"text": c.Text})
			}
			out = append(out, traceMessage{Role: "user", Content: content})
		case provider.AssistantMessage:
			out = append(out, traceMessage{Role: "assistant", Content: serializeAssistantContent(m.Content)})
		case provider.ToolResultMessage:
			content := make([]map[string]any, 0, len(m.Content))
			for _, c := range m.Content {
				content = append(content, map[string]any{"text": c.Text})
			}
			out = append(out, traceMessage{
				Role:       "tool_result",
				ToolCallID: m.ToolCallID,
				ToolName:   m.ToolName,
				Content:    content,
				IsError:    m.IsError,
			})
		}
	}
	return out
}

// serializeAssistantContent maps a slice of provider.AssistantContent
// to the design's {"kind": ...} shape per block. Empty signatures are
// omitted so traces stay readable; non-empty ones round-trip verbatim.
func serializeAssistantContent(content []provider.AssistantContent) []map[string]any {
	out := make([]map[string]any, 0, len(content))
	for _, c := range content {
		switch c := c.(type) {
		case provider.TextContent:
			m := map[string]any{"kind": "text", "text": c.Text}
			if c.TextSignature != "" {
				m["signature"] = c.TextSignature
			}
			out = append(out, m)
		case provider.ThinkingContent:
			m := map[string]any{"kind": "thinking", "text": c.Thinking}
			if c.ThinkingSignature != "" {
				m["signature"] = c.ThinkingSignature
			}
			if c.Redacted {
				m["redacted"] = true
			}
			out = append(out, m)
		case provider.ToolCall:
			m := map[string]any{
				"kind":      "tool_call",
				"id":        c.ID,
				"name":      c.Name,
				"arguments": c.Arguments,
			}
			if c.ThoughtSignature != "" {
				m["signature"] = c.ThoughtSignature
			}
			out = append(out, m)
		}
	}
	return out
}

// buildTracePrograms builds the per-iteration `programs[]` slice. The
// `included` flag is decided by scanning the assembled user message for
// each program's exact rendered header — that's the only path that
// distinguishes "output reached context" from "sentineled" or
// "randomly skipped from the exploration slot" without re-running the
// selection logic (which is non-deterministic for zero-exit inactives).
func buildTracePrograms(results []programResult, inactive map[string]struct{}, userMsg provider.UserMessage) []TraceProgram {
	included := includedPrograms(results, userMsg)

	out := make([]TraceProgram, 0, len(results))
	for _, r := range results {
		_, demoted := inactive[r.name]
		phase := ""
		if demoted {
			if r.exitCode != 0 {
				phase = "nonzero"
			} else {
				phase = "random"
			}
		}
		out = append(out, TraceProgram{
			Name:             r.name,
			Exit:             r.exitCode,
			LatencyMs:        r.latency.Milliseconds(),
			Active:           !demoted,
			Included:         included[r.name],
			ExplorationPhase: phase,
			Output:           string(r.output),
			OutputTokens:     r.renderedTokens(),
		})
	}
	return out
}

// includedPrograms scans the user message for each program's exact
// rendered header (`[program=NAME exit=N]\n`). A program is "included"
// iff a TextContent in the message starts with that header — the
// sentinel form `[program=NAME dropped: ...]` does not match, and
// programs randomly skipped from the exploration slot don't appear at
// all.
func includedPrograms(results []programResult, userMsg provider.UserMessage) map[string]bool {
	out := make(map[string]bool, len(results))
	for _, r := range results {
		header := r.header()
		for _, c := range userMsg.Content {
			if strings.HasPrefix(c.Text, header) {
				out[r.name] = true
				break
			}
		}
	}
	return out
}

// serializeToolResults flattens a slice of synthesized tool results into
// the trace's shape. Each provider.ToolResultMessage's content text is
// joined since the tool layer always emits a single text block.
func serializeToolResults(results []provider.ToolResultMessage) []TraceToolResult {
	out := make([]TraceToolResult, 0, len(results))
	for _, r := range results {
		out = append(out, TraceToolResult{
			ToolCallID: r.ToolCallID,
			ToolName:   r.ToolName,
			Content:    provider.JoinText(r.Content),
			IsError:    r.IsError,
		})
	}
	return out
}

// snapshotStats reads every <root>/statistics/<name>.json file and
// returns the map keyed by program name. The snapshot is taken
// post-iteration so it reflects this iteration's EWMA updates — that's
// the version of statistics the next iteration's revision prompt would
// render. Missing dir → nil; individual unreadable files are silently
// skipped, matching loadStatsFor's contract.
func snapshotStats(root string) map[string]*programStats {
	dir := filepath.Join(root, statsDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := map[string]*programStats{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), statsFileExt) {
			continue
		}
		name := strings.TrimSuffix(e.Name(), statsFileExt)
		if s := loadStatsFor(root, name); s != nil {
			out[name] = s
		}
	}
	return out
}
