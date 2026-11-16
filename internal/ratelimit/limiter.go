// Package ratelimit implements a per-key fixed-window limiter in Redis.
package ratelimit

import (
	"context"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// incrWindow increments the counter and sets the TTL atomically. Doing this in
// two commands would leave a permanent key behind if the process died between
// them, locking a caller out forever.
var incrWindow = goredis.NewScript(`
local n = redis.call("INCR", KEYS[1])
if n == 1 then
  redis.call("EXPIRE", KEYS[1], ARGV[1])
end
return n
`)

// Limiter is a fixed-window counter: simple, one round trip, and good enough
// for abuse protection. It allows bursts at window edges by design.
type Limiter struct {
	rdb    *goredis.Client
	limit  int
	window time.Duration
}

// New returns a limiter allowing perMinute requests per key.
func New(rdb *goredis.Client, perMinute int) *Limiter {
	return &Limiter{rdb: rdb, limit: perMinute, window: time.Minute}
}

// Decision is the outcome of one check.
type Decision struct {
	Allowed    bool
	Limit      int
	Remaining  int
	RetryAfter time.Duration
}

// Allow records a request for key and reports whether it is within budget.
func (l *Limiter) Allow(ctx context.Context, key string) (Decision, error) {
	now := time.Now()
	windowStart := now.Truncate(l.window)
	windowKey := fmt.Sprintf("rl:%s:%d", key, windowStart.Unix())

	n, err := incrWindow.Run(ctx, l.rdb, []string{windowKey}, int(l.window.Seconds())).Int64()
	if err != nil {
		return Decision{}, err
	}

	remaining := l.limit - int(n)
	if remaining < 0 {
		remaining = 0
	}
	return Decision{
		Allowed:    n <= int64(l.limit),
		Limit:      l.limit,
		Remaining:  remaining,
		RetryAfter: windowStart.Add(l.window).Sub(now),
	}, nil
}
