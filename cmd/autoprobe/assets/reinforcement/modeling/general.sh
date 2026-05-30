#!/usr/bin/env bash
# Yield nudge: when in-cycle context crosses modelingThresholdTokens, this
# reinforcement is appended to the last tool result so the model sees it
# right before its next response. The dedicated modeling turn that runs
# between work cycles is responsible for actually updating the world model.
# This script's only job is to ask the model to close the cycle so the modeling turn can take over.
set -eu

cat <<EOF
[YIELD]
You have accumulated significant transient context this tool-use cycle.
Continuing to call tools here makes every subsequent inference re-pay for the history
you have accumulated.

Respond with a brief plain-text summary and NO further tool calls.

A dedicated modeling turn will run next and update the world model based
on what just happened, then the next work cycle will start fresh with your
updated world model outputs in context — and you will be able to attack the next sub-problem from a clean slate.
EOF
