# hopper

Minimal and highly experimental agent harness with the goal of exploring *programmatic
context*.

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

`hopper` is a goal-seeking agent effectively doing continuous program synthesis in order to
minimize token usage without sacrificing intelligence.

### The memory lens

Another way to think about `hopper` is as a memory system for agents that is based on
executable programs, rather than static files.

Most memory systems rely on static markdown files to build up a persistent knowledge base.
But like any form of documentation, static knowledge bases can drift from reality. Again,
the challenge is to continuously test the encoded knowledge against a ground truth, but to
do so *out of context*.

Luckily, a markdown "memory" is just a program that happens to only be executable
in-context. Nothing prevents an intelligent agent from encoding its hard earned knowledge
about the environment it is operating in (say a codebase) in a program that is executable
out of context. This is the difference between documenting the layout of a codebase and
calling `ls` and `grep`.

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

At the core of `hopper` is an agent loop like any other. Where it differs is in the
representation of the context. Instead of modeling context as a conversation interspersed
with tool calls, context in `hopper` is a library of installed programs.

In each iteration, each installed program has a certain probability of getting called. When
it does, it contributes its output to the context for that iteration.

Select programs in the library are considered *cornerstones*. These cannot be deleted or
otherwise modified by the agent and they always get called with probability 1.0.

To be clear, `hopper` can still perform regular tool calls. In fact it comes with some
additional tools for modifying the library. Its just that in each iteration, the context
passed to the LLM is constructed entirely from scratch.
