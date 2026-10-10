package apiserve

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCommandPolicyViolation(t *testing.T) {
	cases := []struct {
		name        string
		message     string
		wantName    string
		wantBlocked bool
	}{
		{"plain message", "帮我看看", "", false},
		{"use with prompt", "/use finance-news 帮我看看", "", false},
		{"use bang prefix", "!use finance-news 帮我看看", "", false},
		{"use clear", "/use clear", "", false},
		{"reload", "/reload", "reload", true},
		{"switch with arg", "/switch gpt-4", "switch", true},
		{"stop bang prefix", "!stop", "stop", true},
		{"subagents", "/subagents", "subagents", true},
		// 未知命令一并拦截：详见 Task 7 顶部"刻意收紧"说明。
		{"unknown command", "/useful thing", "useful", true},
		{"empty", "", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, blocked := commandPolicyViolation(tc.message)
			require.Equal(t, tc.wantBlocked, blocked)
			require.Equal(t, tc.wantName, name)
		})
	}
}
