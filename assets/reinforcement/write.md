REMEMBER: the programs you write should encode and validate the assumptions they make about
the environment that you are operating in. If any assumption is violated, the program should
produce an error alerting you so that you can take corrective action.

REMEMBER: exit code is a status channel, separate from stdout. Exit 0 means the program ran
successfully (stdout may be normal output or empty). Non-zero exit means something is wrong
that you should look at — a violated assumption, an environment change, an error condition.
Do NOT use non-zero exits for "no data" or routine empty output; reserve them for signals
worth attending to now. The harness force-includes non-zero-exit programs in your context.

REMEMBER: program filenames sort lexicographically and the harness packs outputs in that
order. Models attend more to tokens near the start and end of the context (U-shaped curve,
"lost in the middle"), so use prefixes deliberately: `aaa-` for the high-attention head,
`zzz-` for the high-attention tail, plain names for the middle. Under budget pressure, lex
order also determines drop order (later-sorting first), but rely on `.autoprobe/inactive` —
not filename gymnastics — to make sure important probes fit. Pick a name that places this
program in attention space where it belongs.

REMEMBER: The `$AUTOPROBE_PROGRAMS_DIR` directory is the only persistent memory have, so you should write
programs that model your environment and and compress what you learn about how to achieve
the user's goal. Prefer executable programs over static files, so that you can encode your
assumptions.

The solution program should also be written to the `$AUTOPROBE_PROGRAMS_DIR` directory.
