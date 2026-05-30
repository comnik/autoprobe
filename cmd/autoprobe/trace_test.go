package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/comnik/autoprobe/internal/provider"
)

// TestTracerEndToEnd drives the agent through one substantive Step with a
// scripted provider and asserts the on-disk trace shape: an HTML page per
// iteration, an index.html listing it, the run-level static assets, and a
// log.jsonl with a `run` line followed by an `iteration` line that points
// at the rendered HTML file.
func TestTracerEndToEnd(t *testing.T) {
	t.Parallel()

	prov := &scriptedProvider{
		responses: []provider.AssistantMessage{
			{
				Model:      "test-model",
				Content:    []provider.AssistantContent{provider.TextContent{Text: "hello"}},
				StopReason: provider.StopEnd,
				Usage:      provider.Usage{InputTokens: 100, OutputTokens: 20},
			},
		},
	}
	a := newTestAgent(t, prov)

	traceDir := filepath.Join(t.TempDir(), "trace")
	tr, err := NewTracer(traceDir)
	if err != nil {
		t.Fatalf("NewTracer: %v", err)
	}
	defer tr.Close()
	if err := tr.WriteRunHeader(RunHeader{
		AutoprobeVersion:    "test",
		ProbeDir:            ".autoprobe",
		Provider:            "scripted",
		Model:               "test-model",
		Goal:                "smoke",
		ContextBudgetTokens: a.ContextBudget(),
	}); err != nil {
		t.Fatalf("WriteRunHeader: %v", err)
	}
	a.SetTracer(tr)

	// After the header, the static assets and a placeholder index should
	// already exist so operators can open the dir mid-run.
	for _, name := range []string{"style.css", "viewer.js", "index.html"} {
		if _, err := os.Stat(filepath.Join(traceDir, name)); err != nil {
			t.Fatalf("%s missing after WriteRunHeader: %v", name, err)
		}
	}

	runSteps(t, a, 1)

	logPath := filepath.Join(traceDir, traceLogFileName)
	f, err := os.Open(logPath)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()

	var lines []map[string]any
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var m map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &m); err != nil {
			t.Fatalf("log line not JSON: %v: %q", err, scanner.Text())
		}
		lines = append(lines, m)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan log: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("log lines: got %d, want 2 (header + iter)", len(lines))
	}
	if lines[0]["kind"] != "run" {
		t.Fatalf("first line kind: got %v, want run", lines[0]["kind"])
	}
	if lines[1]["kind"] != "iteration" {
		t.Fatalf("second line kind: got %v, want iteration", lines[1]["kind"])
	}
	if lines[1]["n"].(float64) != 1 {
		t.Fatalf("iter n: got %v, want 1", lines[1]["n"])
	}
	if lines[1]["stop_reason"] != "end" {
		t.Fatalf("stop_reason: got %v, want end", lines[1]["stop_reason"])
	}
	if lines[1]["file"] != "iter-00001.html" {
		t.Fatalf("iter file pointer: got %v, want iter-00001.html", lines[1]["file"])
	}

	iterPath := filepath.Join(traceDir, "iter-00001.html")
	data, err := os.ReadFile(iterPath)
	if err != nil {
		t.Fatalf("read iter html: %v", err)
	}
	html := string(data)

	// The page must include the scripted assistant text, the program name
	// from the test program library, and the iteration number in the title.
	for _, want := range []string{"hello", "counter", "iteration 1"} {
		if !strings.Contains(html, want) {
			t.Errorf("iter-00001.html missing %q\n--- excerpt ---\n%s", want, excerpt(html, 400))
		}
	}

	// Index should link to the rendered iteration page.
	indexData, err := os.ReadFile(filepath.Join(traceDir, "index.html"))
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	if !strings.Contains(string(indexData), `href="iter-00001.html"`) {
		t.Errorf("index.html missing link to iter-00001.html\n--- excerpt ---\n%s", excerpt(string(indexData), 400))
	}
}

func excerpt(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// TestTracerResetsDir verifies the directory is cleared on each new
// Tracer construction so a prior run's iter files never leak into the
// next one's listing.
func TestTracerResetsDir(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "trace")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "iter-99999.html")
	if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	tr, err := NewTracer(dir)
	if err != nil {
		t.Fatalf("NewTracer: %v", err)
	}
	defer tr.Close()

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale iter file still present after reset: err=%v", err)
	}
}

// TestTracerNilSafe pins that a nil receiver short-circuits cleanly.
// Agent.SetTracer(nil) plus all the call sites must remain safe.
func TestTracerNilSafe(t *testing.T) {
	t.Parallel()
	var tr *Tracer
	if err := tr.WriteRunHeader(RunHeader{}); err != nil {
		t.Fatalf("nil WriteRunHeader: %v", err)
	}
	if err := tr.WriteIteration(IterationTrace{}); err != nil {
		t.Fatalf("nil WriteIteration: %v", err)
	}
}

// TestParseProgramHeader covers the program-block detector used by the
// renderer to split user-message text into program-output cards.
func TestParseProgramHeader(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in      string
		ok      bool
		name    string
		tail    string
		body    string
		nonzero bool
	}{
		{
			in:   "[program=counter exit=0]\nstep=1\n",
			ok:   true,
			name: "counter",
			tail: "exit=0",
			body: "step=1\n",
		},
		{
			in:      "[program=flaky exit=1]\nboom",
			ok:      true,
			name:    "flaky",
			tail:    "exit=1",
			body:    "boom",
			nonzero: true,
		},
		{
			in:      "[program=huge dropped: 12K tokens exceeds remaining budget 4K]\n",
			ok:      true,
			name:    "huge",
			tail:    "dropped: 12K tokens exceeds remaining budget 4K",
			body:    "",
			nonzero: true,
		},
		{
			in: "[YOUR GOAL]\ninvestigate slow startup",
			ok: false,
		},
		{
			in: "plain text",
			ok: false,
		},
	}

	for _, c := range cases {
		name, tail, body, ok := parseProgramHeader(c.in)
		if ok != c.ok {
			t.Errorf("ok mismatch for %q: got %v, want %v", c.in, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if name != c.name || tail != c.tail || body != c.body {
			t.Errorf("parseProgramHeader(%q) = (%q, %q, %q), want (%q, %q, %q)",
				c.in, name, tail, body, c.name, c.tail, c.body)
		}
		gotNonzero := !strings.HasPrefix(tail, "exit=0")
		if gotNonzero != c.nonzero {
			t.Errorf("nonzero classification for %q: got %v, want %v", c.in, gotNonzero, c.nonzero)
		}
	}
}
