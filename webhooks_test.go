package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/TamarindTech/TamarindLOS/services/core/internal/gateway"
)

// receiver is a webhook endpoint under test control.
type receiver struct {
	mu     sync.Mutex
	status int
	got    []map[string]any // decoded bodies
	heads  []http.Header
}

func (rc *receiver) handler(w http.ResponseWriter, r *http.Request) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	m["_body"] = string(body)
	rc.got = append(rc.got, m)
	rc.heads = append(rc.heads, r.Header.Clone())
	w.WriteHeader(rc.status)
}

func (rc *receiver) count() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.got)
}

// TestWebhooks: operations subscribe a URL, the outbox relays events into signed
// deliveries, failures are retried with backoff, filters apply, and removal stops it.
func TestWebhooks(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.g.Webhooks.AllowInsecure = true // httptest listens on loopback
	start := time.Now()
	at := func(d time.Duration) { e.g.Now = func() time.Time { return start.Add(d) } }
	defer func() { e.g.Now = time.Now }()
	at(0)
	pump := func() (int, int) {
		t.Helper()
		n, err := e.g.Relay(ctx)
		if err != nil {
			t.Fatalf("relay: %v", err)
		}
		m, err := e.g.Send(ctx)
		if err != nil {
			t.Fatalf("send: %v", err)
		}
		return n, m
	}

	all := &receiver{status: http.StatusNoContent}
	allSrv := httptest.NewServer(http.HandlerFunc(all.handler))
	defer allSrv.Close()
	flaky := &receiver{status: http.StatusInternalServerError}
	flakySrv := httptest.NewServer(http.HandlerFunc(flaky.handler))
	defer flakySrv.Close()

	// --- Only operations manage subscriptions; destinations are checked. ---
	expect(t, e.call(officer, "webhook.subscribe", map[string]any{"url": allSrv.URL}, nil), "error", "POLICY_DENIED")
	expect(t, e.call(agent, "webhook.list", map[string]any{}, map[string]any{"reason": "curious"}), "error", "POLICY_DENIED")
	expect(t, e.call(ops, "webhook.subscribe", map[string]any{"url": "ftp://x"}, nil), "error", "VALIDATION_FAILED")
	expect(t, e.call(ops, "webhook.subscribe", map[string]any{"url": "https://user:pw@hooks.example.com/x"}, nil), "error", "VALIDATION_FAILED")
	e.g.Webhooks.AllowInsecure = false
	expect(t, e.call(ops, "webhook.subscribe", map[string]any{"url": "http://hooks.example.com/x"}, nil), "error", "VALIDATION_FAILED")
	expect(t, e.call(ops, "webhook.subscribe", map[string]any{"url": "https://10.0.0.8/x"}, nil), "error", "VALIDATION_FAILED")
	expect(t, e.call(ops, "webhook.subscribe", map[string]any{"url": "https://localhost/x"}, nil), "error", "VALIDATION_FAILED")
	e.g.Webhooks.AllowInsecure = true

	r := e.call(ops, "webhook.subscribe", map[string]any{"url": allSrv.URL, "description": "everything"}, nil)
	expect(t, r, "ok", "")
	secret := r.Result["secret"].(string)
	subAll := r.Result["subscription_id"].(string)
	r = e.call(ops, "webhook.subscribe", map[string]any{"url": flakySrv.URL, "event_types": []string{"ApplicationSubmitted"}}, nil)
	expect(t, r, "ok", "")
	subFlaky := r.Result["subscription_id"].(string)
	// The secret is not logged.
	var logged string
	if err := e.withTenant(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT response->'result'->>'secret' FROM tool_invocations WHERE tool = 'webhook.subscribe'
			AND outcome = 'ok' ORDER BY started_at LIMIT 1`).Scan(&logged)
	}); err != nil || logged != "[redacted]" {
		t.Errorf("secret in the invocation log: %q %v", logged, err)
	}

	// --- Events flow: the earlier subscribe events plus an application. ---
	pump()
	before := all.count()
	r = e.call(officer, "application.create", map[string]any{"product_code": "SME-WC", "requested_amount_minor": 1500000,
		"currency": "USD", "requested_term_months": 12}, nil)
	expect(t, r, "ok", "")
	appID := r.Result["application_id"].(string)
	e.call(officer, "party.add", map[string]any{"application_id": appID, "kind": "business", "role": "applicant",
		"legal_name": "Mekong Trading Co.", "registration_number": "0105561234567", "country": "TH"}, nil)
	e.call(officer, "party.add", map[string]any{"application_id": appID, "kind": "individual", "role": "director",
		"legal_name": "Somchai P.", "date_of_birth": "1980-04-02"}, nil)
	expect(t, e.call(officer, "application.submit", map[string]any{"application_id": appID}, nil), "ok", "")
	n, m := pump()
	if n < 4 || m < 4 {
		t.Fatalf("relay %d, send %d", n, m)
	}
	// Every event reached the catch-all receiver, signed; the filtered one got only the submission.
	types := map[string]int{}
	all.mu.Lock()
	for i, g := range all.got[before:] {
		types[g["type"].(string)]++
		h := all.heads[before+i]
		if h.Get("X-Tamarind-Event") != g["type"] || h.Get("X-Tamarind-Delivery") == "" {
			t.Errorf("headers: %+v", h)
		}
		want := "sha256=" + gateway.Sign(secret, h.Get("X-Tamarind-Timestamp"), []byte(g["_body"].(string)))
		if h.Get("X-Tamarind-Signature") != want {
			t.Errorf("bad signature on %s", g["type"])
		}
		if g["aggregate_id"] != appID {
			t.Errorf("aggregate: %+v", g)
		}
	}
	all.mu.Unlock()
	if types["ApplicationCreated"] != 1 || types["PartyAdded"] != 2 || types["ApplicationSubmitted"] != 1 {
		t.Errorf("delivered types: %+v", types)
	}
	if flaky.count() != 1 {
		t.Fatalf("filtered receiver got %d deliveries", flaky.count())
	}

	// --- The failing receiver is retried with backoff, then succeeds. ---
	r = e.call(ops, "webhook.list", map[string]any{}, nil)
	expect(t, r, "ok", "")
	stats := map[string]map[string]any{}
	for _, s := range r.Result["subscriptions"].([]any) {
		m := s.(map[string]any)
		stats[m["subscription_id"].(string)] = sub(m, "deliveries")
	}
	if stats[subFlaky]["pending"] != float64(1) || stats[subAll]["pending"] != float64(0) || stats[subAll]["delivered"] == float64(0) {
		t.Errorf("stats: %+v", stats)
	}
	if _, m := pump(); m != 0 {
		t.Error("retried before the backoff elapsed")
	}
	at(2 * time.Minute)
	flaky.status = http.StatusOK
	if _, m := pump(); m != 1 || flaky.count() != 2 {
		t.Errorf("retry: %d attempts, %d received", m, flaky.count())
	}
	if _, m := pump(); m != 0 {
		t.Error("delivered more than once after success")
	}

	// --- Removal stops deliveries and drops what is pending. ---
	expect(t, e.call(ops, "webhook.remove", map[string]any{"subscription_id": subAll}, nil), "ok", "")
	expect(t, e.call(ops, "webhook.remove", map[string]any{"subscription_id": subAll}, nil), "error", "NOT_FOUND")
	c := all.count()
	expect(t, e.call(officer, "application.withdraw", map[string]any{"application_id": appID, "reason": "test"}, nil), "ok", "")
	pump()
	if all.count() != c {
		t.Error("a removed subscription still received deliveries")
	}
	r = e.call(ops, "webhook.list", map[string]any{}, nil)
	if len(r.Result["subscriptions"].([]any)) != 1 {
		t.Errorf("after removal: %+v", r.Result)
	}
}
