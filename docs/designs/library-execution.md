# Library execution: guardrails and async runtime

## Implementation status

**Guardrails implemented.** The guardrails section below is implemented
in `runPrograms` (`agent.go`) and covered by `programs_test.go`. The
async execution model remains deferred to a follow-up.

## Problem

Today the harness executes the program library in `runPrograms`
(`agent.go`). Each iteration reads `programs/`, spawns every entry under
a single shared `errgroup` (concurrency cap 8), and calls
`exec.CommandContext(gctx, path).CombinedOutput()` per program. The
results — name, exit code, captured output, latency — are then handed
to the context-assembly stage.

This works for well-behaved programs but offers no real protection
against the failure modes the agent will eventually produce on its own:

- **No per-program timeout.** A program with an infinite loop, a hung
  network call, or an `ncurses`-style child process will block the
  iteration indefinitely. The shared `gctx` only cancels on errgroup
  failure, not on individual stragglers.
- **No output cap.** Output is fully buffered into `[]byte`. A program
  that accidentally `cat`s a multi-megabyte log blows out the agent's
  context window — and the per-iteration prompt cache — on the next
  inference.
- **No process-group isolation.** `exec.CommandContext` only kills the
  direct child on cancellation. Detached descendants (a `cmd & disown`,
  a forked daemon) survive and accumulate on the eval host.
- **One bad program can fail the iteration.** A non-`ExitError` spawn
  failure returns from the `errgroup.Go` closure and cancels every
  other program. The iteration then errors out with no per-program
  result row, hiding the failure from the agent.
- **Stdin is inherited from the harness.** A program that mistakenly
  reads stdin hangs until something else kills it.

The bash tool (`tools.go`) already handles the analogous problems —
per-call timeout, output cap with truncation marker, process-group
kill, `WaitDelay` grace period for late writers, distinct error
channels for timeout vs. spawn vs. exit-status failures. The library
runtime needs the same treatment, plus an additional change unique to
library execution: a single straggling program must not block the next
iteration. That second concern is the **async** part of this design
and is described after the guardrails.

## Design

### Guardrails

The invariant `runPrograms` should hold after this change:
**`runPrograms` returns one `programResult` per program, and basically
never fails.** Every failure mode that today bubbles up as a Go error —
timeout, spawn failure, process-group kill, I/O failure on the pipe —
instead produces a result row with an exit code and an output payload
the agent can read. The only conditions that still surface as a
top-level error are ones the agent cannot act on (reading `programsDir`
itself fails, etc.).

#### 1. The header is the status channel

Today `programResult.header()` (`agent.go:1358`) renders as
`[program=NAME exit=CODE]`. With these guardrails the header grows to
carry the run's *status* in addition to (or in place of) the exit
code — the agent sees the most important fact about the run before
reading any output bytes.

Concretely, `programResult` gains a `status` field. Rendering branches
on it:

| status | header shape |
| --- | --- |
| exited normally | `[program=foo exit=0]` |
| exited non-zero | `[program=foo exit=1]` |
| timed out (process group killed) | `[program=foo timed out after 5m; process group killed]` |
| failed to start (exec error) | `[program=foo failed to start: exec format error]` |
| could not be prepared (stat/chmod error) | `[program=foo could not be prepared: chmod: operation not permitted]` |

The exit code only appears when the program actually exited. For
timeout and spawn failure there is no meaningful POSIX exit status, so
the header carries the reason in words instead. This avoids the
"sentinel exit code" approach — the agent doesn't need to learn that
`exit=-2` means timeout; it just reads the header.

Output truncation (next subsection) composes with this by appending to
the header: `[program=foo exit=0; output truncated at 64KB]` or
`[program=foo exit=1; output truncated at 64KB]`. Timeout and
truncation can also compose: `[program=foo timed out after 5m; output
truncated at 64KB; process group killed]`.

The body of the result row depends on the status. For `exited` it is
the captured stdout/stderr exactly as today. For `timed out` it is
whatever bytes arrived before the kill. For `failed to start` and
`could not be prepared` the error text lives in the header (so the
agent reads the cause without scanning the body), and the body is
empty.

#### 2. Per-program timeout (5 minutes)

Each program gets its own `context.WithTimeout` with a 5-minute
default. On expiry the harness kills the program's **process group**,
not just the direct child — the same pattern as `localBashOps.Exec`
(`tools.go:119–153`): set `SysProcAttr.Setpgid = true` on spawn, and
on cancel call `syscall.Kill(-pid, SIGKILL)`. Without this, an agent
program that backgrounds a child leaves orphans on the host.

The result row renders with the timeout header from §1 above; bytes
captured before the kill are preserved in the body so the agent can
see how far the program got.

The timeout is fixed at 5 minutes for now. Library programs are
intended to be cheap and re-runnable; if a program legitimately needs
longer it can be split, cached, or run via the bash tool on demand.

#### 3. Hard output cap (64 KB)

Captured output is bounded at 64 KB per program. Past the cap the
remainder is discarded (no temp-file spill — the bash tool spills
because *that* output is one-shot; library programs run every
iteration, so the next run is the natural place to fix verbose
output).

Truncation is surfaced in the header (§1), not as a marker line at the
end of the body: `[program=foo exit=0; output truncated at 64KB]`.
Putting it in the header means the agent learns about truncation
before reading the body — same reasoning as for timeout — and the
body remains exactly the first 64 KB of captured bytes with nothing
appended.

The exit code is preserved regardless of truncation: the cap only
affects what bytes reach the agent, not whether the program succeeded.

Implementation-wise this is a streaming wrapper around stdout/stderr
similar to `onDataWriter` (`tools.go:160–172`), but with a single
fixed-size buffer that simply stops appending once full and flips a
`truncated` flag on the result.

#### 4. Process-group isolation and `WaitDelay`

Programs are spawned with `SysProcAttr.Setpgid = true` and a
`cmd.WaitDelay` of ~100ms so a late writer holding the pipes open
after the shell exits doesn't stall the harness. This is the same
shape as `localBashOps.Exec` and exists for the same reason: the
process group is the unit of liveness, not the direct child.

#### 5. Close stdin

`cmd.Stdin = nil` for every program. A program that accidentally
reads stdin should EOF immediately rather than hang until the
timeout fires.

This does not constrain the agent's ability to interact with a
program with custom input: the agent can still invoke the program
directly via the `bash` tool with any stdin or arguments it wants
while iterating on the library. Closing stdin only applies to the
harness-driven execution path, where there is no human or agent at
the other end of the pipe to write anything meaningful.

#### 6. Each program contributes a result row, always

`runPrograms` is restructured so the per-program closure
**never returns a Go error**. Every failure path converts into a
result row with the appropriate status (per §1) before returning:

| failure | status | body |
| --- | --- | --- |
| timeout | `timed out` | captured bytes before kill |
| spawn failure (path not executable, missing interpreter, etc.) | `failed to start` | error text from `exec.Command.Start` |
| stat / chmod failure | `could not be prepared` | error text |
| ran to completion, non-zero exit | `exited` (exit=N) | captured stdout/stderr (possibly truncated) |
| ran to completion, zero exit | `exited` (exit=0) | captured stdout/stderr (possibly truncated) |

The `errgroup` is kept (it manages the concurrency cap of 8) but no
goroutine ever returns a non-nil error; `g.Wait()` always returns
`nil`. The top-level error from `runPrograms` is reserved for "we
couldn't read `programsDir` at all" — a harness-level fault, not a
program-level fault.

#### 7. Hashing across the new status field

`hashResults` (`agent.go:1380`) today hashes `(name, exit_code,
output)` to detect idle iterations. With the new status field, the
hash needs to include enough state that a program flipping between
modes — exited → timed out, exited → truncated, timed out with
different captured prefixes — registers as a change rather than
getting eaten by the backoff.

The hashed tuple becomes `(name, status, exit_code, truncated_flag,
output)`. `exit_code` is hashed as `0` when status is not `exited`,
so it contributes a stable but distinct byte to the digest in those
cases. `truncated_flag` is one byte. This preserves the existing
idle-detection contract: the same library producing the same outputs
hashes identically across iterations; any observable change in what
the agent sees in context produces a different hash.

### Async execution

**Deferred.** Once the guardrails above are in place, the next
question is whether the iteration loop should block on the slowest
program in the library or proceed with whatever results are ready,
filling in stale slots from a prior iteration's cache. That decision
interacts with idle detection (the iteration hash in `hashResults`
assumes every program contributes a fresh row) and with how the agent
reasons about "this output is from two iterations ago." A follow-up
revision of this doc will work that out.
