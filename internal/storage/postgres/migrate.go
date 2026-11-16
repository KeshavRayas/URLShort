package postgres

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

// migrationLockID is an arbitrary constant shared by every instance so two
// replicas starting at once cannot race the DDL.
const migrationLockID = 7362517

// Migrate applies the idempotent schema under an advisory lock.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockID)
	}()

	// PgConn().Exec uses the simple protocol, which is what allows the schema
	// file to contain multiple statements.
	results := conn.Conn().PgConn().Exec(ctx, schemaSQL)
	if _, err := results.ReadAll(); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}
