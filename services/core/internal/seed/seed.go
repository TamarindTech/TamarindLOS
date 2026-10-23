// Package seed creates tenants and products for development and demos.
package seed

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/TamarindTech/TamarindLOS/services/core/internal/auth"
	"github.com/TamarindTech/TamarindLOS/services/core/internal/policy"
)

// Run creates each tenant in the registry (if missing) and the pack's product for it.
func Run(ctx context.Context, conn *pgx.Conn, reg *auth.Registry, pack *policy.Pack) error {
	def, err := json.Marshal(pack.ProductData)
	if err != nil {
		return err
	}
	for slug, t := range reg.Tenants {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		var id string
		err = tx.QueryRow(ctx, `INSERT INTO tamarind.tenants (slug, name) VALUES ($1, $2)
			ON CONFLICT (slug) DO UPDATE SET name = EXCLUDED.name RETURNING id::text`, slug, t.Name).Scan(&id)
		if err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('tamarind.tenant_id', $1, true)`, id); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		p := pack.Product
		if _, err := tx.Exec(ctx, `INSERT INTO tamarind.products
			(tenant_id, code, version, name, status, currency, min_amount_minor, max_amount_minor,
			 min_term_months, max_term_months, policy_pack, policy_pack_version, definition)
			VALUES ($1,$2,$3,$4,'active',$5,$6,$7,$8,$9,$10,$11,$12)
			ON CONFLICT (tenant_id, code, version) DO NOTHING`,
			id, p.Code, pack.Version, p.Name, p.Currency, p.AmountMinor.Min, p.AmountMinor.Max,
			p.TermMonths.Min, p.TermMonths.Max, pack.Name, pack.Version, def); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}
