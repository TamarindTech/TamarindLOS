// Package migrations embeds the SQL migrations and applies them in order.
package migrations

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5"
)

//go:embed *.sql
var files embed.FS

// Apply runs every migration that has not been applied yet. It must be called with
// an administrator connection (the owner of the tamarind schema).
func Apply(ctx context.Context, conn *pgx.Conn) ([]string, error) {
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS public.tamarind_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, err
	}
	names, err := fs.Glob(files, "*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	var applied []string
	for _, name := range names {
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM public.tamarind_migrations WHERE version = $1)`, name).Scan(&exists); err != nil {
			return applied, err
		}
		if exists {
			continue
		}
		sql, err := files.ReadFile(name)
		if err != nil {
			return applied, err
		}
		// Migrations manage their own transactions (BEGIN ... COMMIT) and contain several
		// statements, so they run through the simple query protocol.
		if _, err := conn.PgConn().Exec(ctx, string(sql)).ReadAll(); err != nil {
			return applied, fmt.Errorf("%s: %w", name, err)
		}
		if _, err := conn.Exec(ctx, `INSERT INTO public.tamarind_migrations (version) VALUES ($1)`, name); err != nil {
			return applied, err
		}
		applied = append(applied, name)
	}
	return applied, nil
}
