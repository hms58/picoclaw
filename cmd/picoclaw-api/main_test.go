package main

import (
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

// A mistyped subcommand must be rejected, not silently fall through to the root
// RunE — which serves HTTP in the foreground. `picoclaw-api key list` (note the
// missing "s") looked like it would start a server.
//
// The assertion is on the message, not just on "an error": without NoArgs the
// root command would also fail here (it has no config in a test environment),
// so a bare require.Error would pass for the wrong reason.
func TestRootCommand_RejectsAStraySubcommand(t *testing.T) {
	cmd := newCommand()
	cmd.SetArgs([]string{"key", "list"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	err := cmd.Execute()

	require.Error(t, err, "a stray subcommand must not be accepted")
	require.Contains(t, err.Error(), "unknown command",
		"the failure must be argument parsing, not a downstream config error")
}
