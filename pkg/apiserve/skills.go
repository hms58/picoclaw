package apiserve

import (
	"errors"
	"strings"
)

// ErrSkillNotAllowed is returned when a principal requests a skill outside its
// allowed_skills list.
var ErrSkillNotAllowed = errors.New("apiserve: skill not allowed")

// parseUseDirective extracts a leading "/use <skill>" directive from a message.
// It returns has=false for plain messages, for "/use" alone, and for "/use clear".
//
// The directive is recognized through the SAME normalizer the agent uses
// (commands.CommandName), not by a literal "/use" prefix match. That matters for
// security, not style: the agent accepts "/use", "!use", "/USE" and "/use@bot"
// (pkg/agent/agent_command.go:75-78 normalizes the command name), so a literal
// lowercase check would let every other spelling reach the agent as a valid
// forced-skill directive while skipping authorizeSkill entirely — bypassing
// allowed_skills, the only hard skill boundary (spec 事实 16).
func parseUseDirective(message string) (string, bool) {
	trimmed := strings.TrimSpace(message)

	name, ok := commandName(trimmed)
	if !ok || name != useCommandName {
		return "", false
	}

	// fields[0] is the "/use"-form token (possibly "/use@bot"); the skill is the
	// next field, mirroring pkg/agent/agent_command.go:89.
	fields := strings.Fields(trimmed)
	if len(fields) < 2 {
		return "", false
	}

	skill := strings.TrimSpace(fields[1])
	if skill == "" || strings.EqualFold(skill, "clear") || strings.EqualFold(skill, "off") {
		return "", false
	}
	return skill, true
}

// authorizeSkill enforces principal.AllowedSkills. An empty list means the key
// may use any skill installed for the agent. "" and "auto" are not skill names
// and are always allowed.
func authorizeSkill(p *Principal, skill string) error {
	skill = strings.TrimSpace(skill)
	if skill == "" || isAutoSkill(skill) {
		return nil
	}
	if p == nil {
		return ErrSkillNotAllowed
	}
	if len(p.AllowedSkills) == 0 {
		return nil
	}
	for _, allowed := range p.AllowedSkills {
		if strings.EqualFold(strings.TrimSpace(allowed), skill) {
			return nil
		}
	}
	return ErrSkillNotAllowed
}

// buildSkillMessage prefixes the message so AgentLoop's "/use <skill> <prompt>"
// handler arms the skill for the current turn. The agent rewrites the user
// message back to the prompt part, so the LLM never sees the directive.
//
// "" and "auto" mean "no explicit skill": the message is passed through
// untouched and PicoClaw's built-in semantic loading takes over (the system
// prompt always carries the full skill catalog; the model loads SKILL.md via
// read_file when it decides to). See spec 事实 14 / §4.5.
func buildSkillMessage(message, skill string) string {
	skill = strings.TrimSpace(skill)
	if skill == "" || isAutoSkill(skill) {
		return message
	}
	return "/use " + skill + " " + message
}
