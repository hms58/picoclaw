package apiserve

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRecordUsage_AndSumUsageByAgent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	for _, rec := range []UsageRecord{
		{APIKeyID: "key_1", AgentID: "main", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, CreatedAt: now},
		{APIKeyID: "key_1", AgentID: "main", PromptTokens: 20, CompletionTokens: 5, TotalTokens: 25, CreatedAt: now},
		{APIKeyID: "key_1", AgentID: "stock", PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10, CreatedAt: now},
	} {
		require.NoError(t, st.RecordUsage(ctx, rec))
	}

	rows, err := st.SumUsage(ctx, UsageQuery{GroupBy: "agent"})
	require.NoError(t, err)
	require.Len(t, rows, 2)

	byKey := make(map[string]UsageRow, len(rows))
	for _, row := range rows {
		byKey[row.Key] = row
	}
	require.Equal(t, 2, byKey["main"].Requests)
	require.Equal(t, 30, byKey["main"].PromptTokens)
	require.Equal(t, 40, byKey["main"].TotalTokens)
	require.Equal(t, 1, byKey["stock"].Requests)
}

func TestSumUsage_FiltersByAgentAndWindow(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, st.RecordUsage(ctx, UsageRecord{
		APIKeyID: "key_1", AgentID: "main", TotalTokens: 15, CreatedAt: now,
	}))
	require.NoError(t, st.RecordUsage(ctx, UsageRecord{
		APIKeyID: "key_1", AgentID: "main", TotalTokens: 25, CreatedAt: now.Add(-48 * time.Hour),
	}))

	rows, err := st.SumUsage(ctx, UsageQuery{
		AgentID: "main",
		From:    now.Add(-time.Hour),
		To:      now.Add(time.Hour),
		GroupBy: "agent",
	})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, 15, rows[0].TotalTokens)
}
