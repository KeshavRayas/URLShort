package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kesha/urlshortener/internal/auth"
	"github.com/kesha/urlshortener/internal/link"
)

// Store implements link.Store and auth.Store on top of a pgx pool.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wraps an existing pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

const linkColumns = `code, target_url, user_id, created_at, expires_at, clicks`

// Create inserts a link. A duplicate code is reported as link.ErrCodeTaken so
// the caller can retry with a fresh random code.
func (s *Store) Create(ctx context.Context, l link.Link) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO links (code, target_url, user_id, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5)`,
		l.Code, l.TargetURL, l.UserID, l.CreatedAt, l.ExpiresAt)
	if err != nil {
		if isUniqueViolation(err) {
			return link.ErrCodeTaken
		}
		return fmt.Errorf("insert link: %w", err)
	}
	return nil
}

// GetByCode is the resolve query. Expired rows are filtered here so a cache
// miss cannot resurrect a dead link.
func (s *Store) GetByCode(ctx context.Context, code string) (link.Link, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+linkColumns+`
		FROM links
		WHERE code = $1
		  AND deleted_at IS NULL
		  AND (expires_at IS NULL OR expires_at > now())`, code)
	return scanLink(row)
}

// GetByCodeForUser is the owner-facing read: it returns expired links too, so
// the dashboard can show them, but never another user's row.
func (s *Store) GetByCodeForUser(ctx context.Context, code string, userID uuid.UUID) (link.Link, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+linkColumns+`
		FROM links
		WHERE code = $1 AND user_id = $2 AND deleted_at IS NULL`, code, userID)
	return scanLink(row)
}

// ListByUser returns the caller's links, newest first.
func (s *Store) ListByUser(ctx context.Context, userID uuid.UUID, limit, offset int) ([]link.Link, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+linkColumns+`
		FROM links
		WHERE user_id = $1 AND deleted_at IS NULL
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3`, userID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("query links: %w", err)
	}
	defer rows.Close()

	out := make([]link.Link, 0, limit)
	for rows.Next() {
		var l link.Link
		if err := rows.Scan(&l.Code, &l.TargetURL, &l.UserID, &l.CreatedAt, &l.ExpiresAt, &l.Clicks); err != nil {
			return nil, fmt.Errorf("scan link: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate links: %w", err)
	}
	return out, nil
}

// SoftDelete marks a link deleted. RowsAffected is the authorization check:
// zero means the code does not exist or belongs to someone else.
func (s *Store) SoftDelete(ctx context.Context, code string, userID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE links
		SET deleted_at = now()
		WHERE code = $1 AND user_id = $2 AND deleted_at IS NULL`, code, userID)
	if err != nil {
		return fmt.Errorf("soft delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return link.ErrNotFound
	}
	return nil
}

// UserByAPIKeyHash resolves a credential hash to its account.
func (s *Store) UserByAPIKeyHash(ctx context.Context, keyHash string) (auth.User, error) {
	var u auth.User
	err := s.pool.QueryRow(ctx, `
		SELECT id, name FROM users WHERE api_key_hash = $1`, keyHash).
		Scan(&u.ID, &u.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.User{}, auth.ErrNotFound
	}
	if err != nil {
		return auth.User{}, fmt.Errorf("lookup user: %w", err)
	}
	return u, nil
}

func scanLink(row pgx.Row) (link.Link, error) {
	var l link.Link
	err := row.Scan(&l.Code, &l.TargetURL, &l.UserID, &l.CreatedAt, &l.ExpiresAt, &l.Clicks)
	if errors.Is(err, pgx.ErrNoRows) {
		return link.Link{}, link.ErrNotFound
	}
	if err != nil {
		return link.Link{}, fmt.Errorf("scan link: %w", err)
	}
	return l, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
