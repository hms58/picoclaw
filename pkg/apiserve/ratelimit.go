package apiserve

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/time/rate"
)

// RateLimiter keeps one token bucket per API key.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*rate.Limiter
	limits  map[string]string
}

// NewRateLimiter creates an empty limiter.
func NewRateLimiter() *RateLimiter {
	return &RateLimiter{
		buckets: make(map[string]*rate.Limiter),
		limits:  make(map[string]string),
	}
}

// Allow reports whether the key may issue another request. An empty spec means
// unlimited.
func (l *RateLimiter) Allow(keyID, spec string) bool {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return true
	}

	limit, burst, err := parseRateLimitSpec(spec)
	if err != nil {
		return true // a malformed spec must not lock a caller out
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	existing, ok := l.buckets[keyID]
	if !ok || l.limits[keyID] != spec {
		existing = rate.NewLimiter(limit, burst)
		l.buckets[keyID] = existing
		l.limits[keyID] = spec
	}
	return existing.Allow()
}

// parseRateLimitSpec parses "20/s", "20/s,8" and "600/min".
func parseRateLimitSpec(spec string) (rate.Limit, int, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return 0, 0, fmt.Errorf("empty rate limit spec")
	}

	parts := strings.SplitN(spec, ",", 2)
	ratePart := strings.TrimSpace(parts[0])

	burst := 0
	if len(parts) == 2 {
		parsed, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil || parsed <= 0 {
			return 0, 0, fmt.Errorf("invalid burst in %q", spec)
		}
		burst = parsed
	}

	rateParts := strings.SplitN(ratePart, "/", 2)
	if len(rateParts) != 2 {
		return 0, 0, fmt.Errorf("invalid rate limit spec %q", spec)
	}

	amount, err := strconv.Atoi(strings.TrimSpace(rateParts[0]))
	if err != nil || amount <= 0 {
		return 0, 0, fmt.Errorf("invalid rate amount in %q", spec)
	}

	var perSecond float64
	switch strings.ToLower(strings.TrimSpace(rateParts[1])) {
	case "s", "sec", "second":
		perSecond = float64(amount)
	case "m", "min", "minute":
		perSecond = float64(amount) / 60
	case "h", "hour":
		perSecond = float64(amount) / 3600
	default:
		return 0, 0, fmt.Errorf("invalid rate unit in %q", spec)
	}

	if burst == 0 {
		// Default to the per-second rate, not the raw amount: "600/min" means
		// 10/s, so a burst of 600 would let one caller spend a minute's quota
		// instantly.
		burst = int(perSecond)
		if burst < 1 {
			burst = 1
		}
	}
	return rate.Limit(perSecond), burst, nil
}
