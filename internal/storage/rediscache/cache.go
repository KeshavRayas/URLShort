package rediscache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/kesha/urlshortener/internal/link"
)

const (
	linkPrefix = "link:"
	missPrefix = "miss:"
)

// Cache implements link.Cache on Redis.
type Cache struct {
	rdb *goredis.Client
}

// NewCache wraps a connected client.
func NewCache(rdb *goredis.Client) *Cache {
	return &Cache{rdb: rdb}
}

// Get returns a cached link. A miss is (zero, false, nil), not an error.
func (c *Cache) Get(ctx context.Context, code string) (link.Cached, bool, error) {
	raw, err := c.rdb.Get(ctx, linkPrefix+code).Bytes()
	if errors.Is(err, goredis.Nil) {
		return link.Cached{}, false, nil
	}
	if err != nil {
		return link.Cached{}, false, err
	}
	var v link.Cached
	if err := json.Unmarshal(raw, &v); err != nil {
		// A corrupt value is treated as a miss; the next write repairs it.
		return link.Cached{}, false, nil
	}
	return v, true, nil
}

// Set stores a link with jitter so a large population of keys does not expire
// in one synchronized wave.
func (c *Cache) Set(ctx context.Context, code string, value link.Cached, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode cache value: %w", err)
	}
	return c.rdb.Set(ctx, linkPrefix+code, raw, jitter(ttl)).Err()
}

// Delete evicts a link. Called on delete so revocation is immediate.
func (c *Cache) Delete(ctx context.Context, code string) error {
	return c.rdb.Del(ctx, linkPrefix+code).Err()
}

// GetMiss reports whether a recent lookup already established that the code
// does not exist.
func (c *Cache) GetMiss(ctx context.Context, code string) (bool, error) {
	n, err := c.rdb.Exists(ctx, missPrefix+code).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// SetMiss records a negative lookup for a short window.
func (c *Cache) SetMiss(ctx context.Context, code string, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	return c.rdb.Set(ctx, missPrefix+code, "1", ttl).Err()
}

// DeleteMiss clears the negative entry. Creating a link with a code that was
// probed before it existed must not leave it invisible for the miss window.
func (c *Cache) DeleteMiss(ctx context.Context, code string) error {
	return c.rdb.Del(ctx, missPrefix+code).Err()
}

func jitter(ttl time.Duration) time.Duration {
	if ttl < 20*time.Second {
		return ttl
	}
	spread := int64(ttl / 10)
	return ttl + time.Duration(rand.Int64N(spread*2)-spread)
}
