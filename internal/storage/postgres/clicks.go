package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kesha/urlshortener/internal/click"
)

// RecordClick appends an event and bumps the denormalized counter in one
// transaction, so the dashboard count and the event log cannot drift apart.
func (s *Store) RecordClick(ctx context.Context, ev click.Event) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin click tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO click_events (code, clicked_at, referer, user_agent, visitor_hash)
		VALUES ($1, $2, $3, $4, $5)`,
		ev.Code, ev.ClickedAt, ev.Referer, ev.UserAgent, ev.VisitorHash); err != nil {
		return fmt.Errorf("insert click: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE links SET clicks = clicks + 1 WHERE code = $1`, ev.Code); err != nil {
		return fmt.Errorf("increment clicks: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit click: %w", err)
	}
	return nil
}

// SeedUser is a bootstrap account with an already-hashed key.
type SeedUser struct {
	Name    string
	KeyHash string
}

// BootstrapUsers creates the configured accounts if they do not exist. It is
// idempotent, so restarting the service does not duplicate them.
func BootstrapUsers(ctx context.Context, pool *pgxpool.Pool, users []SeedUser) error {
	for _, u := range users {
		_, err := pool.Exec(ctx, `
			INSERT INTO users (id, name, api_key_hash)
			VALUES ($1, $2, $3)
			ON CONFLICT (api_key_hash) DO UPDATE SET name = EXCLUDED.name`,
			uuid.New(), u.Name, u.KeyHash)
		if err != nil {
			return fmt.Errorf("bootstrap user %q: %w", u.Name, err)
		}
	}
	return nil
}
