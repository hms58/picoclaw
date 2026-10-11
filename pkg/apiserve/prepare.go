package apiserve

import "net/http"

// prepareMessage applies the command policy and the skill authorization to a
// request, then returns the message to hand to the agent.
//
// It writes the error response itself and reports ok=false when the request must
// stop. The step order is load-bearing:
//  1. command policy, judged on the caller's ORIGINAL message — judging the
//     prefixed one would let the skill field shield a caller-supplied command;
//  2. the structured skill field;
//  3. a "/use <name>" embedded in the message (the only hard skill boundary);
//  4. only then build the "/use <skill> " prefix.
func (s *Server) prepareMessage(
	w http.ResponseWriter,
	r *http.Request,
	principal *Principal,
	req *messagesRequest,
	message string,
) (string, bool) {
	if name, blocked := commandPolicyViolation(message); blocked {
		writeError(w, http.StatusBadRequest, codeCapabilityMissing,
			"command /"+name+" is not available through the API", r)
		return "", false
	}

	if err := authorizeSkill(principal, req.Skill); err != nil {
		writeError(w, http.StatusForbidden, codeSkillNotAllowed, "skill not allowed", r)
		return "", false
	}

	if embedded, has := parseUseDirective(message); has {
		if err := authorizeSkill(principal, embedded); err != nil {
			writeError(w, http.StatusForbidden, codeSkillNotAllowed, "skill not allowed", r)
			return "", false
		}
	}

	return buildSkillMessage(message, req.Skill), true
}
