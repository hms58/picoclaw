package apiserve

import (
	"strings"

	"github.com/sipeed/picoclaw/pkg/commands"
)

// useCommandName is the only built-in command the API surface lets through.
const useCommandName = "use"

// autoSkill asks PicoClaw to pick a skill semantically.
const autoSkill = "auto"

// commandName returns the normalized command name of a message, if present.
// It covers both "/" and "!" prefixes (pkg/commands/request.go:20,44-56).
func commandName(message string) (string, bool) {
	return commands.CommandName(message)
}

// commandPolicyViolation reports the offending command name when a message
// carries a command prefix other than "/use".
//
// Spec §6.3 requires the 13 built-in commands to be blocked except /use. This
// blocks *any* command-prefixed message except /use, which is a deliberate
// tightening: an unknown command like "/useful x" would otherwise be answered
// by the agent with the useless text "Unknown command: useful". Do NOT relax
// this back to a 13-name allowlist.
func commandPolicyViolation(message string) (string, bool) {
	name, ok := commandName(message)
	if !ok {
		return "", false
	}
	if name == useCommandName {
		return "", false
	}
	return name, true
}

// isAutoSkill reports whether the requested skill means "let the agent choose".
func isAutoSkill(skill string) bool {
	return strings.EqualFold(strings.TrimSpace(skill), autoSkill)
}
