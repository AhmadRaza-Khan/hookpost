package hookpost

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

const migrateLockID int64 = 7_311_983_441 // arbitrary, constant

// Migrate creates or upgrades hookpost's tables. It is safe to call on every
// startup and from several instances at once (it takes an advisory lock).
//
// Teams that manage DDL with their own tool can instead copy
// migrations/*.sql into it and never call Migrate.
func (c *Client) Migrate(ctx context.Context) error {
	conn, err := c.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrateLockID); err != nil {
		return err
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", migrateLockID)
	}()

	var current int
	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT to_regclass('hookpost_schema_version') IS NOT NULL`).Scan(&exists); err != nil {
		return err
	}
	if exists {
		if err := conn.QueryRow(ctx,
			`SELECT COALESCE(MAX(version), 0) FROM hookpost_schema_version`).Scan(&current); err != nil {
			return err
		}
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, e := range entries {
		v, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("hookpost: bad migration name %q", e.Name())
		}
		if v <= current {
			continue
		}
		sql, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return err
		}
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("hookpost: migration %s: %w", e.Name(), err)
		}
		if _, err := conn.Exec(ctx,
			`INSERT INTO hookpost_schema_version (version) VALUES ($1) ON CONFLICT DO NOTHING`, v); err != nil {
			return fmt.Errorf("hookpost: record migration %s: %w", e.Name(), err)
		}
	}
	return nil
}
