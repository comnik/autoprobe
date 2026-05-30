# autoprobe

Experimental agent harness where context is not formed through append-only conversation, but
*programmatically*, meaning from the output of programs that continuously probe the
environment. Like any other codebase, these probes can be written and evolved by the agent
entirely on its own, or in collaboration with human users. Probes outlive the session they
were created in, allowing the agent to build up a persistent, self-validating model of its
environment.

The hope is that proactive programmatic context offers a more robust and token-efficient
alternative to passive memory systems based on static files, that rely on constant
in-context drift correction. Human-authored probes may also open up new ways of grounding
and steering an agent.

> [!NOTE]
> *This README and files in the `docs/` directory are 100% human written. All other files
> are AI-assisted with a human (me) in the loop reviewing every change. The whole project is
> experimental.*

## Motivation

> From `.md` to `.sh`.

Intelligence is going to be energy constrained. In-context tokens consume orders of
magnitude more energy than traditional deterministic computation, so it can be much more
efficient to trade out-of-context compute if it helps reduce in-context tokens.

To save in-context tokens, agents are commonly encouraged to compress the information they
learn from multiple rounds of tool calling into a persistent knowledge base, often in the
form of static Markdown or HTML documents. From those, subsequent sessions can recover the
appropriate context in a (hopefully!) smaller number of tool calls with a higher
signal-to-noise ratio. `ls` and `grep` calls collapse into a documented directory structure
and an architecture diagram. Repeated trial and error with an unknown CLI are replaced by
concise usage instructions.

But like any form of documentation, static knowledge bases can go stale as the environment
changes. The challenge is to have the agent <ins>continuously compress what it learns about
the environment without constantly spending in-context tokens re-validating its experience
against ground truth</ins>.

`autoprobe` is an exploration of a self-validating memory system based on executable
programs instead of static files.

This simple change means that a "memory" can do both: return a compressed representation
_and_ validate the assumptions it's based on against the current state of the environment. A
description of a component in a codebase also ensures that the relevant source files still
exist at the expected locations. A list of architectural invariants also validate the
dependency graph. Instead of ingesting thousands of tokens of logs, or relying on stale
information from a conversation history, the status of a server or the number of failing
tests in a suite can be checked with probes that simply return `SERVER_STATUS: UP` or `10/10
tests passing` in the happy case, taking up minimal space in-context. Violations escalate
into the context window, where the agent can attend to them and take corrective action. The
context becomes a live dashboard of sensors that is always a reflection of reality.

## Architecture

At the core of `autoprobe` is [an agent loop like any
other](https://ampcode.com/notes/how-to-build-an-agent). Where it differs is in the
representation of the context. Instead of modeling context as a conversation interspersed
with tool calls, the `autoprobe` harness continuously re-constructs the context from
scratch, by assembling the outputs of a library of installed programs.

Currently, the library is just a directory in the local filesystem. Files in that directory
are assumed to be executable. The harness keeps executing all installed programs and
assembles their outputs into the context for the next inference pass, in a way that balances
prompt cache friendliness and liveness.

`autoprobe init` sets up an `.autoprobe` folder in your working directory, which contains
the library along with a few pre-installed system programs which form the system prompt.

Human users can contribute their own programs to the library, or edit those created by the
agent. Typically, at least one human-provided program is used to set (and verify!) the
overall goal to work towards. For simple goals, this can be specified inline via the
`autoprobe run --goal ...` argument.

To be clear: `autoprobe` can still perform regular tool calls. The difference is really just
that the context can change for environmental reasons, without the agent taking any action.
Established tools like `read`, `write`, `edit`, and `bash` are also how the agent is
expected to update the library.

How eagerly to rebuild the context is an area of active experimentation. On one extreme,
rebuilding before every inference pass is too noisy and risks prompt cache invalidations
within a single tool call. Even rebuilding before the current tool-using cycle is completed
might be too aggressive. 

Assistant messages and tool results are retained only while the model is mid tool-using
cycle; once it produces a response with no tool calls the cycle ends and the next iteration
starts fresh with just the library outputs. When the reconstructed context matches the
previous one byte-for-byte (programs produced identical output and nothing new has
happened), the harness idles instead of triggering a redundant inference pass.

The agent currently never auto-terminates; quit with `q` in the TUI or set a budget.

![Workflow](workflow.png)

## Usage

```
autoprobe init
```

This launches an interactive picker for the model provider (Anthropic, OpenAI, Google, or
xAI Grok) and a specific model, then creates `.autoprobe/`:

- `config.yaml` — the chosen provider and model
- `programs/` — the library
- `reinforcement/` — reinforcement messages appended to tool results
- `system/` - programs that form the system prompt

Skip the picker by passing both flags:

```
autoprobe init --provider openai --model gpt-5-codex
```

Re-running `init` on an existing directory refreshes the embedded assets and preserves your
config unless you override it via flags or the picker.

Set the appropriate API key for your chosen provider:

- Anthropic: `ANTHROPIC_API_KEY`
- OpenAI: `OPENAI_API_KEY`
- Google: `GEMINI_API_KEY` or `GOOGLE_API_KEY`
- Grok (xAI): `XAI_API_KEY`

Then run the agent:

```
autoprobe run                            # run autoprobe on the .autoprobe/ directory
autoprobe run --goal "make tests pass"   # inline goal
```

If you don't set a goal or don't provide a library program that does, then the agent will
likely just explore its environment for a bit, create a few probes, and start idling.

## Evaluation

[ProgramBench](https://programbench.com/) instances are used as a first testing ground for
`autoprobe`. The `evals` directory contains scripts to setup a
[sprite](https://sprites.dev/), run an evaluation on it, and download the resulting
`autoprobe` traces (which are human readable HTML pages).

## FAQ

**Q: How is this different from having an agent write skills or tools?**

Installed programs are automatically executed by the harness and so have a chance to feed
information from the environment to the agent proactively. Skills also hard-code the
progressive disclosure mechanism, whereas with `autoprobe` the agent can evolve its own.

**Q: Can I use `autoprobe` with my favourite model?**

`autoprobe` supports Anthropic Claude, OpenAI (including Codex), Google Gemini, and xAI
Grok. Pick one when running `autoprobe init` (or pass `--provider` and `--model` to skip the
picker). Broad compatiblity is not a goal at this stage, so providers may be untested.

**Q: Can I use `autoprobe` with my favourite coding harness?**

The `autoprobe` interaction model might be hard to tack onto a conventional harness via
plugin / skill (let me know if you figure it out!). However programmatic context is a simple
idea and easy to implement, so open source harnesses like
[pi](https://github.com/earendil-works/pi) could easily be forked and adapted.