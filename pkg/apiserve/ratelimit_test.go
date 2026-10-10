package apiserve

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseRateLimitSpec(t *testing.T) {
	limit, burst, err := parseRateLimitSpec("20/s")
	require.NoError(t, err)
	require.Equal(t, 20.0, float64(limit))
	require.Equal(t, 20, burst, "burst defaults to the per-second rate")

	limit, burst, err = parseRateLimitSpec("20/s,8")
	require.NoError(t, err)
	require.Equal(t, 20.0, float64(limit))
	require.Equal(t, 8, burst)

	limit, burst, err = parseRateLimitSpec("600/min")
	require.NoError(t, err)
	require.Equal(t, 10.0, float64(limit))
	require.Equal(t, 10, burst)

	_, _, err = parseRateLimitSpec("")
	require.Error(t, err)

	_, _, err = parseRateLimitSpec("lots")
	require.Error(t, err)
}

func TestRateLimiter_AllowsUpToBurstThenRejects(t *testing.T) {
	limiter := NewRateLimiter()

	for i := 0; i < 2; i++ {
		require.True(t, limiter.Allow("key_1", "1/s,2"), "request %d must pass", i+1)
	}
	require.False(t, limiter.Allow("key_1", "1/s,2"))

	// A different key has its own bucket.
	require.True(t, limiter.Allow("key_2", "1/s,2"))

	// An empty spec means unlimited.
	require.True(t, limiter.Allow("key_3", ""))
}
