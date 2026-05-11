REMEMBER: the programs you write should encode and validate the assumptions they make about
the environment that you are operating in. If any assumption is violated, the program should
produce an error alerting you so that you can take corrective action.

REMEMBER: exit code is a status channel, separate from stdout. Exit 0 means the program ran
successfully (stdout may be normal output or empty). Non-zero exit means something is wrong
that you should look at — a violated assumption, an environment change, an error condition.
Do NOT use non-zero exits for "no data" or routine empty output; reserve them for signals
worth attending to now. The harness force-includes non-zero-exit programs in your context.

REMEMBER: The `$AUTOPROBE_PROGRAMS_DIR` directory is the only persistent memory have, so you should write
programs that model your environment and and compress what you learn about how to achieve
the user's goal. Prefer executable programs over static files, so that you can encode your
assumptions.

The solution program should also be written to the `$AUTOPROBE_PROGRAMS_DIR` directory.
