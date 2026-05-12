package main

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/comnik/autoprobe/internal/provider"
)

func TestEWMASeedsOnFirstObservation(t *testing.T) {
	t.Parallel()
	// n==0 means no prior observation; the EWMA must take the raw value
	// rather than blend a fresh observation with zero, otherwise the
	// first reading sits at α·obs instead of obs.
	if got := ewma(0, 5, 0, 0.1); got != 5 {
		t.Fatalf("first observation should seed the EWMA at the value; got %v", got)
	}
	// Subsequent observations blend at α.
	if got := ewma(5, 15, 1, 0.1); !approxEqual(got, 0.1*15+0.9*5) {
		t.Fatalf("expected α-blended value 6.0, got %v", got)
	}
}

func TestChangeAmountIdenticalIsZero(t *testing.T) {
	t.Parallel()
	if got := changeAmount([]byte("a\nb\nc\n"), []byte("a\nb\nc\n")); got != 0 {
		t.Fatalf("identical outputs should score 0 change; got %v", got)
	}
}

func TestChangeAmountFullyDisjointIsOne(t *testing.T) {
	t.Parallel()
	// No lines in common → LCS = 0 → similarity = 0 → change = 1.
	if got := changeAmount([]byte("a\nb\n"), []byte("x\ny\n")); got != 1 {
		t.Fatalf("disjoint outputs should score 1 change; got %v", got)
	}
}

func TestChangeAmountIsSymmetric(t *testing.T) {
	t.Parallel()
	a := []byte("one\ntwo\nthree\nfour\n")
	b := []byte("one\nTWO\nthree\nFOUR\n")
	if got, alt := changeAmount(a, b), changeAmount(b, a); !approxEqual(got, alt) {
		t.Fatalf("changeAmount must be symmetric; got %v vs %v", got, alt)
	}
}

func TestChangeAmountTrailingNewlineDoesNotInflateCount(t *testing.T) {
	t.Parallel()
	// "a\nb" and "a\nb\n" should score the same — the trailing '\n'
	// shouldn't produce a phantom empty line that pretends to change.
	one := changeAmount([]byte("a\nb"), []byte("a\nb"))
	two := changeAmount([]byte("a\nb\n"), []byte("a\nb\n"))
	if one != two {
		t.Fatalf("trailing newline inflated the change metric: %v vs %v", one, two)
	}
}

func TestOverlapRecallExactMatch(t *testing.T) {
	t.Parallel()
	prog := "the quick brown fox jumps over the lazy dog"
	resp := "I noticed the quick brown fox jumps over the lazy dog in the output"
	if got := overlapWithResponse(prog, resp); !approxEqual(got, 1.0) {
		t.Fatalf("full overlap should score 1.0; got %v", got)
	}
}

func TestOverlapRecallNoMatch(t *testing.T) {
	t.Parallel()
	if got := overlapWithResponse("aaa bbb ccc ddd", "xxx yyy zzz www"); got != 0 {
		t.Fatalf("no shared trigrams should score 0; got %v", got)
	}
}

func TestOverlapRecallShortInputsScoreZero(t *testing.T) {
	t.Parallel()
	// Fewer than 3 words on either side produces no trigrams.
	if got := overlapWithResponse("only two", "the assistant responded with this whole sentence"); got != 0 {
		t.Fatalf("program with <3 words should score 0; got %v", got)
	}
	if got := overlapWithResponse("plenty of words available here for trigrams", "short"); got != 0 {
		t.Fatalf("response with <3 words should score 0; got %v", got)
	}
}

func TestOverlapRecallIsCaseInsensitive(t *testing.T) {
	t.Parallel()
	a := overlapWithResponse("Hello world from autoprobe", "i see hello world from autoprobe today")
	b := overlapWithResponse("hello world from autoprobe", "I SEE HELLO WORLD FROM AUTOPROBE TODAY")
	if !approxEqual(a, b) {
		t.Fatalf("overlap should be case-insensitive; got %v vs %v", a, b)
	}
}

func TestSaveLoadStatsRoundtripPerProgram(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	want := &programStats{
		AvgOutputTokens: 123.4,
		ChangeFrequency: 0.5,
		AvgChangeAmount: 0.25,
		AvgLatencyMs:    7.0,
		Staleness:       3,
		OverlapWithResp: 0.6,
		Samples:         12,
	}
	if err := saveStatsFor(root, "aaa-foo", want); err != nil {
		t.Fatalf("save: %v", err)
	}
	// File must live under statistics/<name>.json — the revision script
	// globs that path, so the location is load-bearing.
	if _, err := os.Stat(filepath.Join(root, "statistics", "aaa-foo.json")); err != nil {
		t.Fatalf("expected statistics/aaa-foo.json on disk: %v", err)
	}
	got := loadStatsFor(root, "aaa-foo")
	if got == nil {
		t.Fatal("expected non-nil stats")
	}
	if *got != *want {
		t.Fatalf("roundtrip mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

func TestLoadStatsForMissingReturnsNil(t *testing.T) {
	t.Parallel()
	if got := loadStatsFor(t.TempDir(), "never-existed"); got != nil {
		t.Fatalf("expected nil for missing file; got %+v", got)
	}
}

func TestLoadStatsForCorruptReturnsNil(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "statistics")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not valid json"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := loadStatsFor(root, "broken"); got != nil {
		t.Fatalf("corrupt file must return nil, not a partial record; got %+v", got)
	}
}

func TestPruneStatsDropsOrphans(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := saveStatsFor(root, "alive", &programStats{Samples: 1}); err != nil {
		t.Fatal(err)
	}
	if err := saveStatsFor(root, "ghost", &programStats{Samples: 1}); err != nil {
		t.Fatal(err)
	}
	if err := pruneStats(root, map[string]struct{}{"alive": {}}); err != nil {
		t.Fatalf("pruneStats: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "statistics", "alive.json")); err != nil {
		t.Errorf("live program's stats should survive prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "statistics", "ghost.json")); !os.IsNotExist(err) {
		t.Errorf("orphan stats file should be removed; stat err=%v", err)
	}
}

func TestPruneStatsLeavesNonJSONFilesAlone(t *testing.T) {
	t.Parallel()
	// The agent may park a README or sidecar note inside statistics/.
	// pruneStats must only touch .json files so those side files survive.
	root := t.TempDir()
	dir := filepath.Join(root, "statistics")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	note := filepath.Join(dir, "README.md")
	if err := os.WriteFile(note, []byte("agent notes"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := pruneStats(root, map[string]struct{}{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(note); err != nil {
		t.Errorf("non-JSON file was removed by prune: %v", err)
	}
}

func TestUpdateStatsWritesPerProgramFiles(t *testing.T) {
	t.Parallel()
	a := newAgentWithPrograms(t,
		programSpec{"aaa", "#!/bin/sh\necho first-program\n"},
		programSpec{"bbb", "#!/bin/sh\necho second-program\n"},
	)
	results, err := a.runPrograms(context.Background())
	if err != nil {
		t.Fatalf("runPrograms: %v", err)
	}
	a.updateStats(results, "the assistant ignored everything")

	for _, name := range []string{"aaa", "bbb"} {
		s := loadStatsFor(a.root, name)
		if s == nil {
			t.Fatalf("expected stats file for %s", name)
		}
		if s.Samples != 1 {
			t.Errorf("%s: expected Samples=1 after first observation, got %d", name, s.Samples)
		}
		if s.AvgOutputTokens <= 0 {
			t.Errorf("%s: AvgOutputTokens should be populated, got %v", name, s.AvgOutputTokens)
		}
	}
}

func TestUpdateStatsTracksChangeOverConsecutiveIterations(t *testing.T) {
	t.Parallel()
	// Run a static program twice — ChangeFrequency seed (n==0) makes
	// the first observation produce ChangeFrequency=0 (no prev), and
	// the second sees prev==curr so it confirms no change. Staleness
	// must tick to 1 on the second iteration to prove the counter is
	// being incremented.
	a := newAgentWithPrograms(t, programSpec{"static", "#!/bin/sh\necho stable\n"})

	results, err := a.runPrograms(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a.updateStats(results, "")
	s1 := loadStatsFor(a.root, "static")
	if s1 == nil || s1.Staleness != 0 {
		t.Fatalf("after first observation, staleness should be 0; got %+v", s1)
	}

	results2, err := a.runPrograms(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a.updateStats(results2, "")
	s2 := loadStatsFor(a.root, "static")
	if s2 == nil {
		t.Fatal("missing stats after second observation")
	}
	if s2.Staleness != 1 {
		t.Errorf("staleness should tick to 1 when output is unchanged; got %d", s2.Staleness)
	}
	if s2.ChangeFrequency != 0 {
		t.Errorf("change frequency should remain 0 for unchanged output; got %v", s2.ChangeFrequency)
	}
	if s2.Samples != 2 {
		t.Errorf("samples should be 2 after two updates; got %d", s2.Samples)
	}
}

func TestUpdateStatsResetsStalenessOnChange(t *testing.T) {
	t.Parallel()
	// counterProgram emits a different value each invocation, so the
	// second iteration sees changed=true and Staleness must stay at 0.
	a := newTestAgent(t, nil)

	r1, err := a.runPrograms(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a.updateStats(r1, "")
	r2, err := a.runPrograms(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a.updateStats(r2, "")

	s := loadStatsFor(a.root, "counter")
	if s == nil {
		t.Fatal("missing stats for counter")
	}
	if s.Staleness != 0 {
		t.Errorf("staleness should reset to 0 on change; got %d", s.Staleness)
	}
	if s.ChangeFrequency <= 0 {
		t.Errorf("change frequency should be > 0 after a change; got %v", s.ChangeFrequency)
	}
	if s.AvgChangeAmount <= 0 {
		t.Errorf("avg change amount should be > 0 after a change; got %v", s.AvgChangeAmount)
	}
}

func TestUpdateStatsPrunesOrphans(t *testing.T) {
	t.Parallel()
	a := newAgentWithPrograms(t, programSpec{"alive", "#!/bin/sh\necho hi\n"})
	// Seed a ghost stats file as if a previously-installed program
	// were still on disk.
	if err := saveStatsFor(a.root, "ghost", &programStats{Samples: 5}); err != nil {
		t.Fatal(err)
	}

	results, err := a.runPrograms(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a.updateStats(results, "")

	if _, err := os.Stat(filepath.Join(a.root, "statistics", "ghost.json")); !os.IsNotExist(err) {
		t.Errorf("ghost stats should be pruned after updateStats; stat err=%v", err)
	}
}

func TestUpdateStatsIsSafeUnderParallelism(t *testing.T) {
	t.Parallel()
	// Several programs at once. The point isn't to detect concrete
	// races — that's go test -race — but to exercise the parallel path
	// so prevOutputs locking and per-file writes don't crash on the
	// happy path. With -race this also catches map/slice misuse.
	specs := make([]programSpec, 0, 12)
	for i := 0; i < 12; i++ {
		specs = append(specs, programSpec{
			name: prefix("p", i),
			body: "#!/bin/sh\necho output\n",
		})
	}
	a := newAgentWithPrograms(t, specs...)

	results, err := a.runPrograms(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a.updateStats(results, "assistant response that does not match")
	for _, s := range specs {
		if loadStatsFor(a.root, s.name) == nil {
			t.Errorf("expected stats file for %s", s.name)
		}
	}
}

func TestOverlapRecordedAgainstAssistantText(t *testing.T) {
	t.Parallel()
	// A program whose output the assistant quotes back: the recall
	// should land at 1.0 EWMA-seeded since this is the first observation.
	a := newAgentWithPrograms(t, programSpec{"qq", "#!/bin/sh\necho the quick brown fox jumps over the lazy dog\n"})
	results, err := a.runPrograms(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a.updateStats(results, "I see the quick brown fox jumps over the lazy dog in your output")
	s := loadStatsFor(a.root, "qq")
	if s == nil {
		t.Fatal("missing stats")
	}
	if !approxEqual(s.OverlapWithResp, 1.0) {
		t.Errorf("expected overlap≈1.0 for quoted output; got %v", s.OverlapWithResp)
	}
}

func TestStatsFileIsValidJSON(t *testing.T) {
	t.Parallel()
	// The revision script's python expects valid JSON. Round-trip
	// through saveStatsFor then re-parse with the standard library to
	// guard against future regressions (e.g. adding NaN/Inf fields).
	root := t.TempDir()
	in := &programStats{AvgOutputTokens: 1, ChangeFrequency: 0.5, AvgChangeAmount: 0.2, AvgLatencyMs: 3, Staleness: 2, OverlapWithResp: 0.1, Samples: 4}
	if err := saveStatsFor(root, "p", in); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "statistics", "p.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("stats file is not parseable JSON: %v\n%s", err, raw)
	}
	for _, field := range []string{"avg_output_tokens", "change_frequency", "avg_change_amount", "avg_latency_ms", "staleness", "overlap_with_response", "samples"} {
		if _, ok := out[field]; !ok {
			t.Errorf("expected field %q in JSON, got keys %v", field, mapKeys(out))
		}
	}
}

func TestJoinAssistantTextSkipsToolCalls(t *testing.T) {
	t.Parallel()
	// Tool-call arguments are structured JSON, not narration; including
	// them would let filenames the model echoes back inflate overlap.
	got := joinAssistantText([]provider.AssistantContent{
		provider.TextContent{Text: "first line"},
		provider.ToolCall{Name: "bash", Arguments: []byte(`{"command":"ls /quick/brown/fox/jumps"}`)},
		provider.TextContent{Text: "second line"},
	})
	if strings.Contains(got, "quick/brown/fox") {
		t.Errorf("tool-call arguments leaked into joined text: %q", got)
	}
	if !strings.Contains(got, "first line") || !strings.Contains(got, "second line") {
		t.Errorf("expected both text segments in joined output: %q", got)
	}
}

// approxEqual compares two floats with a tolerance suitable for the
// scales these tests work at. Avoids over-counting float jitter as a
// test failure.
func approxEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func prefix(base string, n int) string {
	// Generates p00, p01, ... so lex order matches creation order.
	if n < 10 {
		return base + "0" + string(rune('0'+n))
	}
	return base + string(rune('0'+n/10)) + string(rune('0'+n%10))
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
