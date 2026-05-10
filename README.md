# autoprobe

Experimental agent harness with the goal of exploring a shift from passive memory to
*programmatic context*.

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

`autoprobe` is a goal-seeking agent harness that encourages continuous program synthesis in
order to minimize token usage without sacrificing intelligence.

### The memory lens

Another way to think about `autoprobe` is as a memory system for agents that is based on
executable programs, rather than static files.

Most memory systems rely on static markdown files to build up a persistent knowledge base.
But like any form of documentation, static knowledge bases can drift from reality. Again,
the challenge is to continuously test the encoded knowledge against a ground truth, but to
do so *out of context*.

Luckily, a markdown "memory" is just a program that happens to only be executable
in-context. Nothing prevents an intelligent agent from encoding its hard earned knowledge
about the environment it is operating in (say a codebase) in a program that is executable
out of context. This is the difference between documenting the layout of a codebase and
calling `ls` or `grep`.

The key is to encourage the agent to write its "memory programs" in such a way that they
encode and validate their assumptions. For example, knowledge about a specific component in
a codebase should come with a check that ensures that the component still exists at the
expected location. Knowledge about the architecture of the codebase should come with a check
of the dependency graph.

## Objectives

1. 10x fewer in-context tokens without loss of quality or time taken to complete a task.
2. No more compaction.
3. Abstractions that persist across sessions.

## Architecture

At the core of `autoprobe` is an agent loop like any other. Where it differs is in the
representation of the context. Instead of modeling context as a conversation interspersed
with tool calls, the `autoprobe` harness constructs the context from scratch on every
iteration, by assembling the outputs of a library of installed programs. The library is just
a directory in the local filesystem. Files in that directory are assumed to be executable.
In each iteration, the harness executes every installed program and appends the output to
the context for that model call.

`autoprobe init` pre-installs a *cornerstone* program which explains the approach.

Human users can contribute their own programs to the library, or edit those created by the
agent. Typically, at least one human-provided program is used to set (and verify!) the
overall goal to work towards. For simple goals, this can be specified inline via the
`autoprobe run --goal ...` argument.

To be clear, `autoprobe` can still perform regular tool calls. The difference is really just
that in each iteration, the context passed to the LLM is constructed entirely from scratch.
Established tools like `read`, `write`, `edit`, and `bash` are also how the agent is
expected to update the library.

Execution continues until the model no longer returns any tool calls.

## FAQ

**Q: How is this different from having an agent write skills or tools?**

The installed programs are automatically executed on every iteration and so have a chance to
feed information from the environment to the agent pro-actively. Skills also hard-code the
progressive disclosure mechanism, whereas autoprobe can evolve its own.