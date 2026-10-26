package gateway

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Outbound webhooks. Every event already lands in the outbox when it is committed.
// The relay fans each outbox row out into one delivery per matching subscription,
// and the sender posts deliveries with backoff. A delivery carries the event as JSON,
// signed with the subscription's secret:
//
//	X-Tamarind-Event:      <event type>
//	X-Tamarind-Delivery:   <delivery id>
//	X-Tamarind-Timestamp:  <unix seconds>
//	X-Tamarind-Signature:  sha256=<hex HMAC-SHA256 of "<timestamp>.<body>">
//
// A receiver verifies the signature, rejects stale timestamps and treats the delivery
// id as the idempotency key: a delivery may arrive more than once.

// WebhookPolicy says which destinations a tenant may subscribe.
type WebhookPolicy struct {
	// AllowInsecure permits http:// and private or loopback addresses (development only).
	AllowInsecure bool
}

// Delivery schedule: attempt n (1-based) is retried after backoff[n-1]; after the last
// entry the delivery is failed.
var webhookBackoff = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour}

const webhookTimeout = 10 * time.Second

// --- Subscription tools ---------------------------------------------------------------

func (g *Gateway) webhookSubscribe(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	raw := str(c.input, "url")
	if e := g.checkWebhookURL(raw); e != nil {
		return nil, e
	}
	types := nonNilSlice(stringList(c.input["event_types"]))
	secret, err := newSecret()
	if err != nil {
		return nil, err
	}
	id := newUUID()
	// The signing key is shown once here and kept to sign deliveries; it never appears
	// in webhook.list, events or the invocation log (the result is redacted).
	if _, err := tx.Exec(ctx, `INSERT INTO webhook_subscriptions (id, tenant_id, url, description, event_types, secret, created_by)
		VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7)`, id, c.tenantID, raw, str(c.input, "description"), types, secret, c.p.ID); err != nil {
		return nil, err
	}
	c.events = append(c.events, Event{Type: "WebhookSubscribed", AggregateType: "webhook", AggregateID: id, AggregateVersion: 1,
		Data: map[string]any{"subscription_id": id, "url": raw, "event_types": nonNilSlice(types)}})
	return map[string]any{"subscription_id": id, "secret": secret, "url": raw, "event_types": nonNilSlice(types)}, nil
}

func (g *Gateway) webhookList(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	rows, err := tx.Query(ctx, `SELECT s.id::text, s.url, COALESCE(s.description, ''), s.event_types, s.enabled, s.created_by, s.created_at,
		count(d.id) FILTER (WHERE d.status = 'pending'), count(d.id) FILTER (WHERE d.status = 'delivered'),
		count(d.id) FILTER (WHERE d.status = 'failed'), max(d.delivered_at)
		FROM webhook_subscriptions s LEFT JOIN webhook_deliveries d ON d.subscription_id = s.id
		WHERE s.removed_at IS NULL GROUP BY s.id ORDER BY s.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, u, desc, by string
		var types []string
		var enabled bool
		var created time.Time
		var pending, delivered, failed int64
		var last *time.Time
		if err := rows.Scan(&id, &u, &desc, &types, &enabled, &by, &created, &pending, &delivered, &failed, &last); err != nil {
			return nil, err
		}
		item := map[string]any{"subscription_id": id, "url": u, "event_types": nonNilSlice(types), "enabled": enabled,
			"created_by": by, "created_at": created.UTC().Format(time.RFC3339),
			"deliveries": map[string]any{"pending": pending, "delivered": delivered, "failed": failed}}
		if desc != "" {
			item["description"] = desc
		}
		if last != nil {
			item["last_delivered_at"] = last.UTC().Format(time.RFC3339)
		}
		out = append(out, item)
	}
	return map[string]any{"subscriptions": out}, rows.Err()
}

func (g *Gateway) webhookRemove(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	id := str(c.input, "subscription_id")
	tag, err := tx.Exec(ctx, `UPDATE webhook_subscriptions SET enabled = false, removed_at = now() WHERE id = $1 AND removed_at IS NULL`, id)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, newErr(ErrNotFound, "subscription %s not found", id)
	}
	// Nothing more goes out: pending deliveries are dropped.
	if _, err := tx.Exec(ctx, `UPDATE webhook_deliveries SET status = 'failed', last_error = 'subscription removed'
		WHERE subscription_id = $1 AND status = 'pending'`, id); err != nil {
		return nil, err
	}
	v, err := nextVersion(ctx, tx, "webhook", id)
	if err != nil {
		return nil, err
	}
	c.events = append(c.events, Event{Type: "WebhookRemoved", AggregateType: "webhook", AggregateID: id, AggregateVersion: v,
		Data: map[string]any{"subscription_id": id}})
	return map[string]any{"subscription_id": id, "status": "removed"}, nil
}

// checkWebhookURL accepts https destinations on public hosts, without credentials.
// With AllowInsecure (development) http and local addresses are fine too.
func (g *Gateway) checkWebhookURL(raw string) *ToolError {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return newErr(ErrValidation, "url must be an absolute http(s) URL")
	}
	if u.User != nil {
		return newErr(ErrValidation, "url must not carry credentials")
	}
	if g.Webhooks.AllowInsecure {
		return nil
	}
	if u.Scheme != "https" {
		return newErr(ErrValidation, "url must use https")
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil && isPrivate(ip) {
		return newErr(ErrValidation, "url must not point at a private or local address")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return newErr(ErrValidation, "url must not point at a local address")
	}
	return nil
}

func isPrivate(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

func newSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "whsec_" + hex.EncodeToString(b[:]), nil
}

// --- Relay and sender ---------------------------------------------------------------

// RunOutbox relays and sends until ctx ends.
func (g *Gateway) RunOutbox(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if n, err := g.Relay(ctx); err != nil {
			g.Log.Warn("outbox relay failed", "err", err)
		} else if n > 0 {
			g.Log.Info("outbox relay", "deliveries", n)
		}
		if n, err := g.Send(ctx); err != nil {
			g.Log.Warn("webhook sender failed", "err", err)
		} else if n > 0 {
			g.Log.Info("webhooks sent", "attempts", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Relay turns unpublished outbox rows into deliveries, one per subscription whose
// filter matches the event, and marks the rows published. It returns the number of
// deliveries created. Each row is its own transaction; a row claimed by another
// replica is skipped.
func (g *Gateway) Relay(ctx context.Context) (int, error) {
	total := 0
	var errs []error
	for _, tenant := range g.tenantIDs {
		ids, err := g.candidates(ctx, tenant, `SELECT id::text FROM outbox WHERE published_at IS NULL ORDER BY id LIMIT 200`)
		if err != nil {
			errs = append(errs, fmt.Errorf("tenant %s: %w", tenant, err))
			continue
		}
		for _, id := range ids {
			n, err := g.relayOne(ctx, tenant, id)
			total += n
			if err != nil {
				errs = append(errs, fmt.Errorf("tenant %s outbox %s: %w", tenant, id, err))
				break
			}
		}
	}
	return total, errors.Join(errs...)
}

func (g *Gateway) relayOne(ctx context.Context, tenant, outboxID string) (int, error) {
	n := 0
	err := withTenantTx(ctx, g.DB, tenant, func(tx pgx.Tx) error {
		var eventID, typ string
		err := tx.QueryRow(ctx, `SELECT o.event_id::text, e.type FROM outbox o JOIN events e ON e.id = o.event_id
			WHERE o.id = $1 AND o.published_at IS NULL FOR UPDATE OF o SKIP LOCKED`, outboxID).Scan(&eventID, &typ)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id::text FROM webhook_subscriptions
			WHERE enabled AND removed_at IS NULL AND (cardinality(event_types) = 0 OR $1 = ANY(event_types))`, typ)
		if err != nil {
			return err
		}
		subs, err := collectIDs(rows, nil)
		if err != nil {
			return err
		}
		for _, sub := range subs {
			tag, err := tx.Exec(ctx, `INSERT INTO webhook_deliveries (tenant_id, subscription_id, event_id, next_attempt_at)
				VALUES ($1,$2,$3,$4) ON CONFLICT (subscription_id, event_id) DO NOTHING`, tenant, sub, eventID, g.Now().UTC())
			if err != nil {
				return err
			}
			n += int(tag.RowsAffected())
		}
		_, err = tx.Exec(ctx, `UPDATE outbox SET published_at = $2, attempts = attempts + 1 WHERE id = $1`, outboxID, g.Now().UTC())
		return err
	})
	return n, err
}

// Send posts every delivery that is due, recording the outcome and the next attempt.
// It returns the number of attempts made.
func (g *Gateway) Send(ctx context.Context) (int, error) {
	total := 0
	var errs []error
	for _, tenant := range g.tenantIDs {
		ids, err := g.candidates(ctx, tenant, `SELECT id::text FROM webhook_deliveries
			WHERE status = 'pending' AND next_attempt_at <= $1 ORDER BY next_attempt_at LIMIT 100`, g.Now().UTC())
		if err != nil {
			errs = append(errs, fmt.Errorf("tenant %s: %w", tenant, err))
			continue
		}
		for _, id := range ids {
			if ctx.Err() != nil {
				return total, ctx.Err()
			}
			sent, err := g.sendOne(ctx, tenant, id)
			if sent {
				total++
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("tenant %s delivery %s: %w", tenant, id, err))
			}
		}
	}
	return total, errors.Join(errs...)
}

// delivery is what the sender needs for one attempt.
type delivery struct {
	id, url, secret, eventType string
	attempts                   int
	body                       []byte
}

func (g *Gateway) sendOne(ctx context.Context, tenant, id string) (bool, error) {
	// Claim the delivery in a short transaction, so a slow receiver never holds a lock.
	var d delivery
	err := withTenantTx(ctx, g.DB, tenant, func(tx pgx.Tx) error {
		var ev struct {
			ID, Type, Actor, AggregateType, AggregateID string
			Version                                     int
			OccurredAt                                  time.Time
			Data                                        []byte
			Seq                                         int64
		}
		err := tx.QueryRow(ctx, `SELECT d.id::text, s.url, s.secret, d.attempts, e.id::text, e.type, e.actor, e.aggregate_type,
			e.aggregate_id::text, e.aggregate_version, e.occurred_at, e.data, e.seq
			FROM webhook_deliveries d JOIN webhook_subscriptions s ON s.id = d.subscription_id JOIN events e ON e.id = d.event_id
			WHERE d.id = $1 AND d.status = 'pending' AND d.next_attempt_at <= $2 AND s.enabled FOR UPDATE OF d SKIP LOCKED`,
			id, g.Now().UTC()).Scan(&d.id, &d.url, &d.secret, &d.attempts, &ev.ID, &ev.Type, &ev.Actor, &ev.AggregateType,
			&ev.AggregateID, &ev.Version, &ev.OccurredAt, &ev.Data, &ev.Seq)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNothingToDo
		}
		if err != nil {
			return err
		}
		d.eventType = ev.Type
		d.body, _ = json.Marshal(map[string]any{"event_id": ev.ID, "seq": ev.Seq, "type": ev.Type, "actor": ev.Actor,
			"aggregate_type": ev.AggregateType, "aggregate_id": ev.AggregateID, "aggregate_version": ev.Version,
			"occurred_at": ev.OccurredAt.UTC().Format(time.RFC3339Nano), "data": json.RawMessage(ev.Data)})
		// Hold the slot while the request runs: the next attempt time moves past the
		// timeout, so another replica does not pick the same delivery up meanwhile.
		_, err = tx.Exec(ctx, `UPDATE webhook_deliveries SET next_attempt_at = $2 WHERE id = $1`, id,
			g.Now().UTC().Add(webhookTimeout+time.Minute))
		return err
	})
	if errors.Is(err, errNothingToDo) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	status, sendErr := g.post(ctx, d)
	attempt := d.attempts + 1
	return true, withTenantTx(ctx, g.DB, tenant, func(tx pgx.Tx) error {
		now := g.Now().UTC()
		if sendErr == nil {
			_, err := tx.Exec(ctx, `UPDATE webhook_deliveries SET status = 'delivered', attempts = $2, last_status = $3,
				last_error = NULL, delivered_at = $4 WHERE id = $1`, id, attempt, status, now)
			return err
		}
		if attempt > len(webhookBackoff) {
			_, err := tx.Exec(ctx, `UPDATE webhook_deliveries SET status = 'failed', attempts = $2, last_status = NULLIF($3, 0),
				last_error = $4 WHERE id = $1`, id, attempt, status, sendErr.Error())
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE webhook_deliveries SET attempts = $2, last_status = NULLIF($3, 0), last_error = $4,
			next_attempt_at = $5 WHERE id = $1`, id, attempt, status, sendErr.Error(), now.Add(webhookBackoff[attempt-1]))
		return err
	})
}

// post makes one signed delivery. It returns the HTTP status (0 when no response).
func (g *Gateway) post(ctx context.Context, d delivery) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, webhookTimeout)
	defer cancel()
	ts := strconv.FormatInt(g.Now().Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(d.body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "tamarind-webhooks/0.1")
	req.Header.Set("X-Tamarind-Event", d.eventType)
	req.Header.Set("X-Tamarind-Delivery", d.id)
	req.Header.Set("X-Tamarind-Timestamp", ts)
	req.Header.Set("X-Tamarind-Signature", "sha256="+Sign(d.secret, ts, d.body))
	client := g.WebhookClient
	if client == nil {
		client = &http.Client{Timeout: webhookTimeout}
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return res.StatusCode, fmt.Errorf("receiver answered %d", res.StatusCode)
	}
	return res.StatusCode, nil
}

// Sign computes the hex HMAC-SHA256 a receiver should compare against.
func Sign(secret, timestamp string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(timestamp))
	m.Write([]byte("."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}
