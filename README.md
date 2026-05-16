# autoprobe

Experimental agent harness where context is constructed by executable programs that
constantly probe the environment. Like any other codebase, these programs are written and
evolved by the agent entirely on its own, or in collaboration with human users. This active
*programmatic context* then becomes an alternative to passive memory systems based on static
files.

**Objectives**

1. 10x fewer in-context tokens without loss of quality or time taken to complete a task.
2. Reusable abstractions that persist across sessions. No hard compaction.
3. Fine-grained grounding and steering by human users.

## Motivation

Intelligence is going to be energy constrained. In-context tokens consume orders of
magnitude more energy than tokens flowing through traditional deterministic programs. So it
is generally much more efficient for an intelligent agent to hard-code the reasoning steps
required to solve a problem into a traditional program, rather than actually solve it
in-context.

But in-context is where the magic happens, where the agent can course correct if an
assumption suddenly clashes with reality. The challenge is to find a balance, where the
generated programs continuously validate their core assumptions against ground truth and
escalate back to the agent if any assumption is violated.

If a traditional agent needs to know whether all tests are passing, or whether a server is
up, it may read thousands of tokens of logs, or rely on stale information from the
conversation history. Wrapped in `autoprobe`, an agent should write a script that checks and
outputs `SERVER_STATUS: UP` or `10/10 tests passing`.

I wanted a goal-seeking agent harness that encourages program synthesis combined with
continuous probing of the environment, in order to minimize token usage without sacrificing
intelligence.

### The memory lens

> From `.md` to `.sh`.

Another way to think about `autoprobe` is as a memory system for agents that is based on
executable programs, rather than static files.

Most memory systems rely on static markdown files to build up a persistent knowledge base.
But like any form of documentation, static knowledge bases can drift from reality. Again,
the challenge is to continuously test the encoded knowledge against a ground truth, but to
do so *out of context*.

A markdown "memory" is just a program that happens to only be executable in-context. Nothing
prevents an intelligent agent from encoding its hard earned knowledge about the environment
it is operating in (say a codebase) in a program that is executable out of context. This is
the difference between documenting the layout of a codebase in a `.md` vs calling `ls` or
`grep`. Both have different strenghts and weaknesses. The `.md` compresses the knowledge but
can drift. Calling `ls` and `grep` always reflects ground truth, but can cause lots of
redundant information to spill into the context window.

So the key is to encourage the agent to write its "memory programs" in such a way that when
executed, they return a compressed representation of the knowledge, but also validate their
underlying assumptions against the current state of the environment. For example, knowledge
about a specific component in a codebase should come with a check that ensures that the
component still exists at the expected location. Knowledge about the architecture of the
codebase should come with a check of the dependency graph.

Instead of an agent that writes a diary of what it did, `autoprobe` agents install probes in
the environment they are operating in. The context window becomes a live dashboard of
sensors that is always a fresh, verified reflection of reality.

## Architecture

At the core of `autoprobe` is [an agent loop like any
other](https://ampcode.com/notes/how-to-build-an-agent). Where it differs is in the
representation of the context. Instead of modeling context as a conversation interspersed
with tool calls, the `autoprobe` harness constructs the context from scratch on every
iteration, by assembling the outputs of a library of installed programs. It is worth it to
spend cheap out-of-context compute in order to improve the signal-to-noise ratio of the
context window.

The library is just a directory in the local filesystem. Files in that directory are assumed
to be executable. In each iteration, the harness executes every installed program and
appends the output to the context for that model call.

`autoprobe init` sets up the library (`.autoprobe/programs` by default) and pre-installs a
*cornerstone* program which explains the approach.

Human users can contribute their own programs to the library, or edit those created by the
agent. Typically, at least one human-provided program is used to set (and verify!) the
overall goal to work towards. For simple goals, this can be specified inline via the
`autoprobe run --goal ...` argument.

To be clear: `autoprobe` can still perform regular tool calls. The difference is really just
that in each iteration, the context passed to the LLM is constructed entirely from scratch.
Established tools like `read`, `write`, `edit`, and `bash` are also how the agent is
expected to update the library.

Each iteration re-runs the programs and rebuilds the user-side context from scratch.
Assistant messages and tool results are retained only while the model is mid tool-using
cycle; once it produces a response with no tool calls the cycle ends and the next
iteration starts fresh with just the new program outputs. When the reconstructed
context matches the previous one byte-for-byte (programs produced identical output
and nothing new has happened), the harness idles with exponential backoff (capped at
30s) instead of re-querying the model. The agent never auto-terminates; quit with `q`
in the TUI.

![Workflow](workflow.png)

## Usage

Initialize an `autoprobe` directory in your project:

```
autoprobe init
```

This launches an interactive picker for the model provider (Anthropic, OpenAI, Google, or
xAI Grok) and a specific model, then creates `.autoprobe/`:

- `config.yaml` — the chosen provider and model
- `programs/` — the cornerstone program plus anything you or the agent install
- `reinforcement/` — per-tool reinforcement messages appended to tool results

Skip the picker by passing both flags:

```
autoprobe init --provider openai --model gpt-5-codex
```

Passing only one of `--provider` / `--model` skips that screen and prompts for the other.

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
autoprobe run --goal "make tests pass"   # inline goal, appended as a final program output
autoprobe run --debug                    # pause between iterations
```

## Evaluation

[ProgramBench](https://programbench.com/) instances are used as a first testing ground for
`autoprobe`. The `evals` directory contains scripts to setup a
[sprite](https://sprites.dev/), run an evaluation on it, and download the resulting
`autoprobe` traces (which are human readable HTML pages).

## FAQ

**Q: How is this different from having an agent write skills or tools?**

The installed programs are automatically executed on every iteration and so have a chance to
feed information from the environment to the agent pro-actively. Skills also hard-code the
progressive disclosure mechanism, whereas with `autoprobe` the agent can evolve its own.

**Q: Can I use `autoprobe` with my favourite model?**

`autoprobe` supports Anthropic Claude, OpenAI (including Codex), Google Gemini, and
xAI Grok. Pick one when running `autoprobe init` (or pass `--provider` and `--model`
to skip the picker). Reasoning / thinking content round-trips across turns for the
first three providers; xAI does not return a replayable reasoning signature, so Grok
runs without thinking continuity (the agent loop tolerates this). Tool calling works
the same way regardless of which provider you choose.

**Q: Can I use `autoprobe` with my favourite coding harness?**

No, the `autoprobe` interaction model can't be tacked on to a conventional harness via
plugin / skill. However programmatic context is a simple idea and easy to implement, so open
source harnesses like the great [pi](https://github.com/earendil-works/pi) could easily be
forked and adapted.