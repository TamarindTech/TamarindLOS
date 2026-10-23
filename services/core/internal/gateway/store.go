package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// withTenantTx runs fn in a transaction scoped to a tenant (row-level security).
func withTenantTx(ctx context.Context, db *pgxpool.Pool, tenantID string, fn func(pgx.Tx) error) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(ctx, `SELECT set_config('tamarind.tenant_id', $1, true)`, tenantID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Event is a domain event to append to the hash-chained log.
type Event struct {
	ID               string
	TenantID         string
	AggregateType    string
	AggregateID      string
	AggregateVersion int
	Type             string
	Actor            string
	OnBehalfOf       string
	OccurredAt       time.Time
	TraceID          string
	Data             any
}

// canonical returns the bytes that are hashed for an event. Data is re-encoded with
// sorted keys and literal numbers, so the same bytes can be rebuilt from the jsonb
// column when the chain is verified.
func canonical(ev Event, data json.RawMessage) ([]byte, error) {
	norm, err := normalizeJSON(data)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"id":                ev.ID,
		"tenant_id":         ev.TenantID,
		"aggregate_type":    ev.AggregateType,
		"aggregate_id":      ev.AggregateID,
		"aggregate_version": ev.AggregateVersion,
		"type":              ev.Type,
		"actor":             ev.Actor,
		"on_behalf_of":      ev.OnBehalfOf,
		"occurred_at":       ev.OccurredAt.UTC().Format(time.RFC3339Nano),
		"trace_id":          ev.TraceID,
		"data":              norm,
	})
}

func normalizeJSON(b []byte) (json.RawMessage, error) {
	if len(b) == 0 {
		return json.RawMessage("{}"), nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	out, err := json.Marshal(v)
	return out, err
}

// appendEvent writes an event and its outbox row. Events are chained per tenant:
// hash = SHA-256(prev_hash || canonical(event)). An advisory lock serialises
// appends per tenant so the chain cannot fork.
func appendEvent(ctx context.Context, tx pgx.Tx, ev Event) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "events:"+ev.TenantID); err != nil {
		return err
	}
	var prev []byte
	err := tx.QueryRow(ctx, `SELECT hash FROM events WHERE tenant_id = $1 ORDER BY seq DESC LIMIT 1`, ev.TenantID).Scan(&prev)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if ev.ID == "" {
		ev.ID = newUUID()
	}
	ev.OccurredAt = ev.OccurredAt.UTC().Truncate(time.Microsecond)
	data, err := json.Marshal(ev.Data)
	if err != nil {
		return err
	}
	if string(data) == "null" {
		data = []byte("{}")
	}
	data, err = normalizeJSON(data)
	if err != nil {
		return err
	}
	c, err := canonical(ev, data)
	if err != nil {
		return err
	}
	h := sha256.New()
	h.Write(prev)
	h.Write(c)
	sum := h.Sum(nil)

	if _, err := tx.Exec(ctx, `INSERT INTO events
		(id, tenant_id, aggregate_type, aggregate_id, aggregate_version, type, actor, on_behalf_of, occurred_at, trace_id, data, prev_hash, hash)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9,NULLIF($10,''),$11,$12,$13)`,
		ev.ID, ev.TenantID, ev.AggregateType, ev.AggregateID, ev.AggregateVersion, ev.Type, ev.Actor,
		ev.OnBehalfOf, ev.OccurredAt, ev.TraceID, data, prev, sum); err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO outbox (event_id, tenant_id) VALUES ($1, $2)`, ev.ID, ev.TenantID); err != nil {
		return fmt.Errorf("insert outbox: %w", err)
	}
	return nil
}

// ChainReport is the result of verifying a tenant's audit chain.
type ChainReport struct {
	Events  int    `json:"events"`
	Valid   bool   `json:"valid"`
	BrokeAt string `json:"broke_at,omitempty"` // event id where verification failed
	Head    string `json:"head,omitempty"`     // hex hash of the last event
}

// VerifyChain recomputes every hash in a tenant's event log.
func VerifyChain(ctx context.Context, db *pgxpool.Pool, tenantID string) (ChainReport, error) {
	rep := ChainReport{Valid: true}
	err := withTenantTx(ctx, db, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text, tenant_id::text, aggregate_type, aggregate_id::text, aggregate_version,
			type, actor, COALESCE(on_behalf_of,''), occurred_at, COALESCE(trace_id,''), data::text, prev_hash, hash
			FROM events WHERE tenant_id = $1 ORDER BY seq`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		var prev []byte
		for rows.Next() {
			var ev Event
			var data string
			var prevHash, hash []byte
			if err := rows.Scan(&ev.ID, &ev.TenantID, &ev.AggregateType, &ev.AggregateID, &ev.AggregateVersion,
				&ev.Type, &ev.Actor, &ev.OnBehalfOf, &ev.OccurredAt, &ev.TraceID, &data, &prevHash, &hash); err != nil {
				return err
			}
			rep.Events++
			c, err := canonical(ev, []byte(data))
			if err != nil {
				return err
			}
			h := sha256.New()
			h.Write(prev)
			h.Write(c)
			sum := h.Sum(nil)
			if !bytes.Equal(prevHash, prev) || !bytes.Equal(sum, hash) {
				rep.Valid = false
				rep.BrokeAt = ev.ID
				return nil
			}
			prev = hash
		}
		rep.Head = hex.EncodeToString(prev)
		return rows.Err()
	})
	return rep, err
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
