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
// scripted provider and asserts the on-disk trace shape: the per-iteration
// JSON contains the captured context / response / programs slice and the
// log.jsonl gains a matching iteration line after the run header.
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

	iterPath := filepath.Join(traceDir, "iter-00001.json")
	data, err := os.ReadFile(iterPath)
	if err != nil {
		t.Fatalf("read iter file: %v", err)
	}
	var rec map[string]any
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("iter file not JSON: %v", err)
	}
	if rec["format_version"].(float64) != float64(traceFormatVersion) {
		t.Fatalf("format_version: got %v, want %d", rec["format_version"], traceFormatVersion)
	}
	if rec["iteration"].(float64) != 1 {
		t.Fatalf("iteration: got %v, want 1", rec["iteration"])
	}

	// Context messages: the single user message built from the counter program.
	ctxMsgs := rec["context"].(map[string]any)["messages"].([]any)
	if len(ctxMsgs) != 1 {
		t.Fatalf("context messages: got %d, want 1", len(ctxMsgs))
	}
	first := ctxMsgs[0].(map[string]any)
	if first["role"] != "user" {
		t.Fatalf("first message role: got %v, want user", first["role"])
	}

	// Response: one text block with the scripted reply.
	respContent := rec["response"].(map[string]any)["content"].([]any)
	if len(respContent) != 1 {
		t.Fatalf("response content blocks: got %d, want 1", len(respContent))
	}
	if respContent[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("response text: got %v, want hello", respContent[0].(map[string]any)["text"])
	}

	// Programs slice: the counter program ran, exit 0, included in context.
	programs := rec["programs"].([]any)
	if len(programs) != 1 {
		t.Fatalf("programs: got %d, want 1", len(programs))
	}
	p := programs[0].(map[string]any)
	if p["name"] != "counter" {
		t.Fatalf("program name: got %v, want counter", p["name"])
	}
	if p["active"] != true {
		t.Fatalf("program active: got %v, want true", p["active"])
	}
	if p["included"] != true {
		t.Fatalf("program included: got %v, want true", p["included"])
	}
	if !strings.Contains(p["output"].(string), "step=") {
		t.Fatalf("program output: got %q, want substring step=", p["output"])
	}

	// Budget snapshot.
	budget := rec["budget"].(map[string]any)
	if budget["limit_tokens"].(float64) != float64(a.ContextBudget()) {
		t.Fatalf("limit_tokens: got %v, want %d", budget["limit_tokens"], a.ContextBudget())
	}
	if budget["overflowed"] != false {
		t.Fatalf("overflowed: got %v, want false", budget["overflowed"])
	}

	// Stats snapshot includes counter's record after this iteration's EWMA
	// updates rolled in.
	stats := rec["stats_snapshot"].(map[string]any)
	if _, ok := stats["counter"]; !ok {
		t.Fatalf("stats_snapshot missing counter: got keys %v", keysOf(stats))
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
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
	stale := filepath.Join(dir, "iter-99999.json")
	if err := os.WriteFile(stale, []byte("{}"), 0o644); err != nil {
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

