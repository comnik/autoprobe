package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// findResult looks up a program by name in a result slice. Tests use this
// instead of trusting positional ordering when they install several
// programs and only care about one.
func findResult(t *testing.T, results []programResult, name string) *programResult {
	t.Helper()
	for i := range results {
		if results[i].name == name {
			return &results[i]
		}
	}
	t.Fatalf("no result row for %q (got %d rows)", name, len(results))
	return nil
}

func TestProgramHeaderShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		r    programResult
		want string
	}{
		{
			name: "exited zero",
			r:    programResult{name: "foo", status: programExited, exitCode: 0},
			want: "[program=foo exit=0]\n",
		},
		{
			name: "exited nonzero",
			r:    programResult{name: "foo", status: programExited, exitCode: 1},
			want: "[program=foo exit=1]\n",
		},
		{
			name: "exited zero with truncation",
			r:    programResult{name: "foo", status: programExited, exitCode: 0, truncated: true, outputCap: 64 * 1024},
			want: "[program=foo exit=0; output truncated at 64KB]\n",
		},
		{
			name: "exited nonzero with truncation",
			r:    programResult{name: "foo", status: programExited, exitCode: 1, truncated: true, outputCap: 64 * 1024},
			want: "[program=foo exit=1; output truncated at 64KB]\n",
		},
		{
			name: "timed out",
			r:    programResult{name: "foo", status: programTimedOut, timeoutUsed: 5 * time.Minute},
			want: "[program=foo timed out after 5m0s; process group killed]\n",
		},
		{
			name: "timed out with truncation",
			r:    programResult{name: "foo", status: programTimedOut, timeoutUsed: 5 * time.Minute, truncated: true, outputCap: 64 * 1024},
			want: "[program=foo timed out after 5m0s; process group killed; output truncated at 64KB]\n",
		},
		{
			name: "failed to start with cause",
			r:    programResult{name: "foo", status: programFailedToStart, cause: "exec format error"},
			want: "[program=foo failed to start: exec format error]\n",
		},
		{
			name: "could not be prepared with cause",
			r:    programResult{name: "foo", status: programCouldNotPrepare, cause: "chmod: permission denied"},
			want: "[program=foo could not be prepared: chmod: permission denied]\n",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := c.r.header()
			if got != c.want {
				t.Errorf("header drift:\n got:  %q\n want: %q", got, c.want)
			}
		})
	}
}

func TestSanitizeHeaderTextStripsBreakers(t *testing.T) {
	t.Parallel()
	// Newlines, carriage returns, and ']' bytes inside cause text would
	// break the single-line, bracket-terminated header shape that
	// downstream parsers rely on. The sanitizer must scrub them.
	got := sanitizeHeaderText("first line\nsecond line\rthird ]end")
	if strings.ContainsAny(got, "\n\r]") {
		t.Errorf("sanitized text still contains breakers: %q", got)
	}
	if !strings.Contains(got, "third )end") {
		t.Errorf("expected ']' to be rewritten to ')'; got %q", got)
	}
}

func TestHashResultsFlipsOnStatusChange(t *testing.T) {
	t.Parallel()
	// Two results with the same name, same empty output, but different
	// statuses must hash differently. Otherwise a program that flips
	// from exited-zero to timed-out (or failed-to-start) with no
	// captured output gets eaten by the idle backoff.
	exited := []programResult{{name: "p", status: programExited, exitCode: 0}}
	timedOut := []programResult{{name: "p", status: programTimedOut}}
	failed := []programResult{{name: "p", status: programFailedToStart}}
	if hashResults(exited) == hashResults(timedOut) {
		t.Error("hash should differ when status flips exited→timed_out")
	}
	if hashResults(exited) == hashResults(failed) {
		t.Error("hash should differ when status flips exited→failed_to_start")
	}
	if hashResults(timedOut) == hashResults(failed) {
		t.Error("hash should differ between timed_out and failed_to_start")
	}
}

func TestHashResultsFlipsOnTruncation(t *testing.T) {
	t.Parallel()
	// Same name, status, exit, and prefix bytes — only the truncated
	// flag differs. The hash must register this so a program that
	// starts spilling past the cap is not mistaken for a stable run.
	body := []byte("identical-prefix")
	full := []programResult{{name: "p", status: programExited, output: body, truncated: false}}
	cut := []programResult{{name: "p", status: programExited, output: body, truncated: true}}
	if hashResults(full) == hashResults(cut) {
		t.Error("hash should differ when truncated flag flips")
	}
}

func TestProgramOutputCapStopsAtBoundary(t *testing.T) {
	t.Parallel()
	c := newProgramOutputCap(10)
	if _, err := c.Write([]byte("12345")); err != nil {
		t.Fatal(err)
	}
	if c.didTruncate() {
		t.Fatalf("should not be truncated yet; buf=%q", c.bytes())
	}
	// This write straddles the cap: half lands, half drops.
	if _, err := c.Write([]byte("67890ABCDE")); err != nil {
		t.Fatal(err)
	}
	if got := string(c.bytes()); got != "1234567890" {
		t.Errorf("expected exactly the first 10 bytes; got %q", got)
	}
	if !c.didTruncate() {
		t.Errorf("truncated flag should be set after exceeding cap")
	}
	// Further writes are dropped on the floor, flag stays set.
	if _, err := c.Write([]byte("XXX")); err != nil {
		t.Fatal(err)
	}
	if got := string(c.bytes()); got != "1234567890" {
		t.Errorf("post-cap writes must not extend the buffer; got %q", got)
	}
}

func TestRunProgramsTruncatesAtCap(t *testing.T) {
	t.Parallel()
	a := newAgentWithPrograms(t,
		programSpec{"loud", "#!/bin/sh\nprintf 'A%.0s' $(seq 1 500)\n"},
	)
	// Cap well below the 500-byte output so the program is forced past it.
	a.programMaxOutputBytes = 100

	results, err := a.runPrograms(context.Background())
	if err != nil {
		t.Fatalf("runPrograms: %v", err)
	}
	r := findResult(t, results, "loud")
	if r.status != programExited || r.exitCode != 0 {
		t.Errorf("program ran to completion; want status=exited exit=0, got status=%v exit=%d", r.status, r.exitCode)
	}
	if !r.truncated {
		t.Errorf("truncated flag should be set when output exceeds the cap")
	}
	if len(r.output) != 100 {
		t.Errorf("output must be capped to exactly 100 bytes; got %d", len(r.output))
	}
	if !strings.Contains(r.header(), "output truncated at 100B") {
		t.Errorf("header should advertise the actual cap; got %q", r.header())
	}
}

func TestRunProgramsTimesOut(t *testing.T) {
	t.Parallel()
	// Two-second timeout (rather than something tighter) so the shell
	// reliably finishes booting and writes "starting" before the kill
	// arrives even when this test races dozens of siblings on a busy
	// CI box. The point is to verify the timeout fires and bytes
	// captured before the kill survive — not to measure how snappy
	// the kill is.
	a := newAgentWithPrograms(t,
		programSpec{"slow", "#!/bin/sh\necho starting; sleep 60\n"},
	)
	a.programTimeout = 2 * time.Second

	start := time.Now()
	results, err := a.runPrograms(context.Background())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("runPrograms: %v", err)
	}
	r := findResult(t, results, "slow")
	if r.status != programTimedOut {
		t.Errorf("want status=timed_out, got %v (header=%q)", r.status, r.header())
	}
	// Should have killed close to the timeout, not waited for sleep 60 to finish.
	if elapsed > 10*time.Second {
		t.Errorf("runPrograms waited too long for the timeout; elapsed=%v", elapsed)
	}
	if !strings.Contains(string(r.output), "starting") {
		t.Errorf("partial output captured before the kill should be preserved; got %q", r.output)
	}
	if !strings.Contains(r.header(), "timed out") {
		t.Errorf("header should advertise the timeout; got %q", r.header())
	}
}

func TestRunProgramsHandlesFailedToStart(t *testing.T) {
	t.Parallel()
	// A shebang pointing at a path that almost certainly doesn't exist
	// makes the kernel refuse to spawn the process. The harness must
	// translate that into a failed_to_start result row, not a
	// runPrograms-level error.
	a := newAgentWithPrograms(t,
		programSpec{"bad-shebang", "#!/path/that/does/not/exist/nope\necho should-not-run\n"},
	)

	results, err := a.runPrograms(context.Background())
	if err != nil {
		t.Fatalf("runPrograms must not error on a spawn failure: %v", err)
	}
	r := findResult(t, results, "bad-shebang")
	if r.status != programFailedToStart {
		t.Errorf("want status=failed_to_start, got %v (header=%q)", r.status, r.header())
	}
	if r.cause == "" {
		t.Errorf("expected an inline cause in the result; header=%q", r.header())
	}
	if !strings.Contains(r.header(), "failed to start") {
		t.Errorf("header should advertise failed-to-start; got %q", r.header())
	}
	if len(r.output) != 0 {
		t.Errorf("body should be empty for failed-to-start; got %q", r.output)
	}
}

func TestRunProgramsHandlesCouldNotBePrepared(t *testing.T) {
	t.Parallel()
	// A file inside programs/ that disappears between readDir and the
	// worker's stat — simulate by writing a regular file, then deleting
	// it before runPrograms's stat call. We can't deterministically
	// race the harness, so the cleaner shape here is to drop a dangling
	// symlink instead: readDir returns it, stat fails.
	a := newAgentWithPrograms(t)
	dangling := filepath.Join(a.programsDir, "dangling")
	if err := os.Symlink(filepath.Join(a.programsDir, "no-such-target"), dangling); err != nil {
		t.Fatal(err)
	}

	results, err := a.runPrograms(context.Background())
	if err != nil {
		t.Fatalf("runPrograms must not error on a prepare failure: %v", err)
	}
	r := findResult(t, results, "dangling")
	if r.status != programCouldNotPrepare {
		t.Errorf("want status=could_not_be_prepared, got %v (header=%q)", r.status, r.header())
	}
	if r.cause == "" {
		t.Errorf("expected an inline cause; header=%q", r.header())
	}
	if !strings.Contains(r.header(), "could not be prepared") {
		t.Errorf("header should advertise the failure; got %q", r.header())
	}
}

func TestRunProgramsClosesStdin(t *testing.T) {
	t.Parallel()
	// Programs that read stdin must see EOF immediately, not hang
	// waiting for input the harness can't supply. `cat` reads stdin
	// until EOF; with stdin closed it exits 0 with no output before
	// the timeout fires.
	a := newAgentWithPrograms(t,
		programSpec{"reads-stdin", "#!/bin/sh\ncat\n"},
	)
	a.programTimeout = 2 * time.Second

	results, err := a.runPrograms(context.Background())
	if err != nil {
		t.Fatalf("runPrograms: %v", err)
	}
	r := findResult(t, results, "reads-stdin")
	// Status == exited (rather than timed_out) is the assertion: with
	// stdin closed, cat sees EOF immediately and exits 0. If stdin were
	// inherited from the harness we'd block until the timeout fires.
	if r.status != programExited || r.exitCode != 0 {
		t.Errorf("program reading stdin should exit cleanly on EOF; got status=%v exit=%d header=%q",
			r.status, r.exitCode, r.header())
	}
}

func TestRunProgramsOneFailureDoesNotPoisonOthers(t *testing.T) {
	t.Parallel()
	// The pre-guardrail runPrograms used a shared errgroup context — a
	// single spawn failure would cancel its siblings and the iteration
	// would lose results that should have come through. The new
	// contract is that every file in programsDir gets a result row,
	// regardless of what happened to others.
	a := newAgentWithPrograms(t,
		programSpec{"bad-shebang", "#!/path/that/does/not/exist/nope\n"},
		programSpec{"ok-a", "#!/bin/sh\necho a-ok\n"},
		programSpec{"ok-b", "#!/bin/sh\necho b-ok\n"},
	)

	results, err := a.runPrograms(context.Background())
	if err != nil {
		t.Fatalf("runPrograms: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 result rows, got %d", len(results))
	}
	for _, n := range []string{"ok-a", "ok-b"} {
		r := findResult(t, results, n)
		if r.status != programExited || r.exitCode != 0 {
			t.Errorf("sibling of failed program should still run cleanly; %s: status=%v exit=%d body=%q",
				n, r.status, r.exitCode, r.output)
		}
	}
}
