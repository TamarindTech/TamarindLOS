package gateway

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamarindTech/TamarindLOS/services/core/internal/auth"
)

// The event feed lets subscribers (Tamarind's own agents first) follow a tenant's
// domain events in order. Events are written in the same transaction as the state
// change, and pg_notify fires when that transaction commits, so a long-polling
// subscriber wakes up as soon as there is something new (Reference Architecture §9.2).

// notifyChannel is the PostgreSQL channel used to signal new events. The payload is
// the tenant id; it carries no event data.
const notifyChannel = "tamarind_events"

// FeedEvent is one event as subscribers see it.
type FeedEvent struct {
	Seq              int64           `json:"seq"`
	ID               string          `json:"event_id"`
	Type             string          `json:"type"`
	AggregateType    string          `json:"aggregate_type"`
	AggregateID      string          `json:"aggregate_id"`
	AggregateVersion int             `json:"aggregate_version"`
	Actor            string          `json:"actor"`
	OnBehalfOf       string          `json:"on_behalf_of,omitempty"`
	OccurredAt       time.Time       `json:"occurred_at"`
	Data             json.RawMessage `json:"data"`
}

// FeedPage is a page of the feed. Pass Next as `after` to continue.
type FeedPage struct {
	Events []FeedEvent `json:"events"`
	Next   int64       `json:"next_after"`
}

// hub wakes long-polling readers when a tenant has new events.
type hub struct {
	mu      sync.Mutex
	waiters map[string]chan struct{}
}

func (h *hub) wait(tenantID string) <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.waiters == nil {
		h.waiters = map[string]chan struct{}{}
	}
	ch, ok := h.waiters[tenantID]
	if !ok {
		ch = make(chan struct{})
		h.waiters[tenantID] = ch
	}
	return ch
}

func (h *hub) wake(tenantID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ch, ok := h.waiters[tenantID]; ok {
		close(ch)
		delete(h.waiters, tenantID)
	}
}

// Listen relays PostgreSQL notifications to waiting feed readers until ctx ends.
// Readers also re-check every second, so the feed works (more slowly) without it.
func (g *Gateway) Listen(ctx context.Context) {
	for ctx.Err() == nil {
		if err := g.listenOnce(ctx); err != nil && ctx.Err() == nil {
			g.Log.Warn("event listener stopped; retrying", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (g *Gateway) listenOnce(ctx context.Context) error {
	conn, err := g.DB.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
		return err
	}
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		g.feed.wake(n.Payload)
	}
}

// Events returns up to limit events after seq `after`, waiting up to `wait` for the
// first one. Types, when given, filters by event type.
func (g *Gateway) Events(ctx context.Context, p *auth.Principal, after int64, limit int, types []string, wait time.Duration) (*FeedPage, *ToolError) {
	tenantID := g.tenantIDs[p.Tenant]
	if tenantID == "" {
		return nil, newErr(ErrPolicyDenied, "principal's tenant is not provisioned")
	}
	if ok, missing := p.HasScopes([]string{"events:read"}); !ok {
		return nil, &ToolError{Code: ErrPolicyDenied, Message: "missing scopes", Details: map[string]any{"missing_scopes": missing}}
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if wait > 30*time.Second {
		wait = 30 * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		woken := g.feed.wait(tenantID) // register before reading, so no notification is missed
		page, err := g.readEvents(ctx, tenantID, after, limit, types)
		if err != nil {
			g.Log.Error("event feed", "err", err)
			return nil, newErr(ErrInternal, "internal error")
		}
		remaining := time.Until(deadline)
		if len(page.Events) > 0 || remaining <= 0 {
			return page, nil
		}
		select {
		case <-ctx.Done():
			return page, nil
		case <-woken:
		case <-time.After(min(remaining, time.Second)):
		}
	}
}

func (g *Gateway) readEvents(ctx context.Context, tenantID string, after int64, limit int, types []string) (*FeedPage, error) {
	page := &FeedPage{Events: []FeedEvent{}, Next: after}
	err := withTenantTx(ctx, g.DB, tenantID, func(tx pgx.Tx) error {
		// Appends are serialised per tenant until commit (see appendEvent), so sequence
		// order is commit order and nothing can later appear below head.
		var head *int64
		if err := tx.QueryRow(ctx, `SELECT max(seq) FROM events WHERE tenant_id = $1`, tenantID).Scan(&head); err != nil {
			return err
		}
		if head == nil || *head <= after {
			return nil
		}
		rows, err := tx.Query(ctx, `SELECT seq, id::text, type, aggregate_type, aggregate_id::text, aggregate_version, actor,
			COALESCE(on_behalf_of, ''), occurred_at, data
			FROM events WHERE tenant_id = $1 AND seq > $2 AND seq <= $3 AND (COALESCE(cardinality($4::text[]), 0) = 0 OR type = ANY($4))
			ORDER BY seq LIMIT $5`, tenantID, after, *head, types, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e FeedEvent
			var data []byte
			if err := rows.Scan(&e.Seq, &e.ID, &e.Type, &e.AggregateType, &e.AggregateID, &e.AggregateVersion, &e.Actor,
				&e.OnBehalfOf, &e.OccurredAt, &data); err != nil {
				return err
			}
			e.Data = data
			page.Events = append(page.Events, e)
			page.Next = e.Seq
		}
		if err := rows.Err(); err != nil {
			return err
		}
		// A short page read everything up to head; filtered-out events are skipped too.
		if len(page.Events) < limit {
			page.Next = *head
		}
		return nil
	})
	return page, err
}
