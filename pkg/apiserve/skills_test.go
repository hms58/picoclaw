package apiserve

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseUseDirective(t *testing.T) {
	cases := []struct {
		name      string
		message   string
		wantSkill string
		wantHas   bool
	}{
		{"leading directive with prompt", "/use finance-news 帮我看看", "finance-news", true},
		{"leading directive only", "/use finance-news", "finance-news", true},
		{"padded directive", "   /use   finance-news   问题  ", "finance-news", true},
		{"clear is not a skill", "/use clear", "", false},
		{"prefix without separator is not a directive", "/useful thing", "", false},
		// 以下三种拼写 agent 都认（commands.CommandName 归一化），
		// 接入层必须同样认出来，否则技能授权会被绕过。
		{"bang prefix", "!use evil-skill 问题", "evil-skill", true},
		{"uppercase directive", "/USE evil-skill 问题", "evil-skill", true},
		{"telegram suffix", "/use@bot evil-skill 问题", "evil-skill", true},
		{"bang directive only", "!use", "", false},
		{"plain message", "帮我看看", "", false},
		{"empty", "", "", false},
		{"only command", "/use", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			skill, has := parseUseDirective(tc.message)
			require.Equal(t, tc.wantHas, has)
			require.Equal(t, tc.wantSkill, skill)
		})
	}
}

func TestBuildSkillMessage(t *testing.T) {
	require.Equal(t, "帮我看看", buildSkillMessage("帮我看看", ""))
	require.Equal(t, "/use finance-news 帮我看看", buildSkillMessage("帮我看看", "finance-news"))
	require.Equal(t, "帮我看看", buildSkillMessage("帮我看看", "   "))
	// "auto" 是内建语义加载（catalog + read_file），不做任何转换（spec §4.5）。
	require.Equal(t, "帮我看看", buildSkillMessage("帮我看看", "auto"))
	require.Equal(t, "帮我看看", buildSkillMessage("帮我看看", "AUTO"))
}

func TestAuthorizeSkill(t *testing.T) {
	unrestricted := &Principal{}
	restricted := &Principal{AllowedSkills: []string{"finance-news"}}

	require.NoError(t, authorizeSkill(unrestricted, "anything"))
	require.NoError(t, authorizeSkill(restricted, "finance-news"))
	require.NoError(t, authorizeSkill(restricted, "  Finance-News  "))
	require.ErrorIs(t, authorizeSkill(restricted, "evil-skill"), ErrSkillNotAllowed)
	require.NoError(t, authorizeSkill(restricted, ""), "no skill requested is always allowed")
	// "auto" 不是一个技能名，任何 key 都可用。
	require.NoError(t, authorizeSkill(restricted, "auto"))
}
