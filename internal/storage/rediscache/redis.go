// Package rediscache is the read-through cache in front of Postgres.
package rediscache

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Connect opens a client and waits for the server. Redis is optional at
// startup: if it never comes up the API still serves from Postgres, so the
// caller decides how long to wait.
func Connect(ctx context.Context, addr, password string, db int, log *slog.Logger) (*goredis.Client, error) {
	client := goredis.NewClient(&goredis.Options{
		Addr:         addr,
		Password:     password,
		DB:           db,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
		MaxRetries:   2,
	})

	deadline := time.Now().Add(30 * time.Second)
	for {
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := client.Ping(pingCtx).Err()
		cancel()
		if err == nil {
			return client, nil
		}
		if time.Now().After(deadline) {
			_ = client.Close()
			return nil, fmt.Errorf("connect to redis: %w", err)
		}
		log.Warn("redis not ready; retrying", "err", err)
		select {
		case <-ctx.Done():
			_ = client.Close()
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
