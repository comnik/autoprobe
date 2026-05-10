REMEMBER: the programs you write should encode and validate the assumptions they make about
the environment that you are operating in. If any assumption is violated, the program should
produce an error alerting you so that you can take corrective action.

REMEMBER: The `$AUTOPROBE_PROGRAMS_DIR` directory is the only persistent memory have, so you should write
programs that model your environment and and compress what you learn about how to achieve
the user's goal. Prefer executable programs over static files, so that you can encode your
assumptions.

The solution program should also be written to the `$AUTOPROBE_PROGRAMS_DIR` directory.
