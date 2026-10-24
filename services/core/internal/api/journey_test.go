package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamarindTech/TamarindLOS/services/core/internal/gateway"
)

const (
	verifier = "dev-agent-verification"
	senior   = "dev-approver-sara"
	ops      = "dev-ops-dana"
	borrower = "dev-borrower-mekong"
)

func sub(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}

func (e *env) state(token, appID string) map[string]any {
	e.t.Helper()
	r := e.call(token, "application.get", map[string]any{"application_id": appID}, nil)
	expect(e.t, r, "ok", "")
	return sub(r.Result, "application")
}

// partiesByRole returns party ids keyed by role.
func partiesByRole(app map[string]any) map[string]string {
	out := map[string]string{}
	for _, p := range app["parties"].([]any) {
		m := p.(map[string]any)
		out[m["role"].(string)] = m["party_id"].(string)
	}
	return out
}

// newApplication creates an application with a business applicant and one director.
func (e *env) newApplication(amount int, applicantName string, linkBorrower bool) (appID, applicant, director string) {
	e.t.Helper()
	in := map[string]any{"product_code": "SME-WC", "requested_amount_minor": amount, "currency": "USD",
		"requested_term_months": 12, "purpose": "working capital for peak season"}
	if linkBorrower {
		in["borrower"] = "borrower:mekong"
	}
	r := e.call(officer, "application.create", in, nil)
	expect(e.t, r, "ok", "")
	appID = r.Result["application_id"].(string)
	r = e.call(officer, "party.add", map[string]any{"application_id": appID, "kind": "business", "role": "applicant",
		"legal_name": applicantName, "registration_number": "0105561234567", "country": "TH"}, nil)
	expect(e.t, r, "ok", "")
	applicant = r.Result["party_id"].(string)
	r = e.call(officer, "party.add", map[string]any{"application_id": appID, "kind": "individual", "role": "director",
		"legal_name": "Somchai P.", "date_of_birth": "1980-04-02", "ownership_pct": 60}, nil)
	expect(e.t, r, "ok", "")
	director = r.Result["party_id"].(string)
	r = e.call(officer, "application.submit", map[string]any{"application_id": appID}, nil)
	expect(e.t, r, "ok", "")
	if r.Result["state"] != "intake" {
		e.t.Fatalf("after submit: want intake, got %v", r.Result["state"])
	}
	return
}

var portalClick = map[string]any{"method": "portal_click", "ip": "203.0.113.10"}

// TestLoanJourney takes one SME loan from intake to booking, with people, agents and
// the borrower each doing their part.
func TestLoanJourney(t *testing.T) {
	e := setup(t)
	vr := map[string]any{"reason": "verification run for submitted application"}
	uw := map[string]any{"reason": "underwriting analysis"}

	appID, applicant, director := e.newApplication(5000000, "Mekong Trading Co.", true)
	app := e.state(officer, appID)
	if app["completeness_pct"].(float64) >= 100 || len(app["missing_consents"].([]any)) != 4 {
		t.Fatalf("expected 4 missing consents, got %v / %v", app["completeness_pct"], app["missing_consents"])
	}

	// Checks cannot start before intake is complete.
	r := e.call(verifier, "kyc.run", map[string]any{"application_id": appID, "party_id": director}, vr)
	expect(t, r, "error", "PRECONDITION_FAILED")

	// --- The borrower sees only their own application, and gives consent. ---
	r = e.call(borrower, "application.get", map[string]any{"application_id": appID}, nil)
	expect(t, r, "ok", "")
	otherID, _, _ := e.newApplication(1000000, "Unrelated Co.", false)
	r = e.call(borrower, "application.get", map[string]any{"application_id": otherID}, nil)
	expect(t, r, "error", "POLICY_DENIED")
	r = e.call(borrower, "party.add", map[string]any{"application_id": appID, "kind": "individual", "role": "guarantor", "legal_name": "X"}, nil)
	expect(t, r, "error", "POLICY_DENIED")

	consents := []struct{ party, scope string }{
		{applicant, "kyc"}, {applicant, "credit_bureau"}, {applicant, "bank_data"}, {director, "kyc"},
	}
	for i, c := range consents {
		r = e.call(borrower, "consent.record", map[string]any{"application_id": appID, "party_id": c.party, "scope": c.scope, "evidence": portalClick}, nil)
		expect(t, r, "ok", "")
		want := "intake"
		if i == len(consents)-1 {
			want = "verification" // intake complete: the state machine moves on by itself
		}
		if r.Result["state"] != want {
			t.Fatalf("after consent %d: want %s, got %v", i, want, r.Result["state"])
		}
	}

	// --- The verification agent runs every mandatory check (autonomy L3: executes). ---
	r = e.call(verifier, "credit.pull_report", map[string]any{"application_id": appID, "party_id": director, "report_type": "consumer"}, vr)
	expect(t, r, "error", "CONSENT_MISSING") // no credit_bureau consent from the director
	steps := []struct {
		tool  string
		input map[string]any
	}{
		{"kyc.run", map[string]any{"party_id": director}},
		{"kyc.run", map[string]any{"party_id": applicant}},
		{"sanctions.screen", map[string]any{"party_id": applicant}},
		{"sanctions.screen", map[string]any{"party_id": director}},
		{"credit.pull_report", map[string]any{"party_id": applicant, "report_type": "commercial"}},
		{"bank.analyse_statements", map[string]any{"party_id": applicant, "source": "open_banking"}},
	}
	for _, s := range steps {
		s.input["application_id"] = appID
		r = e.call(verifier, s.tool, s.input, vr)
		expect(t, r, "ok", "")
		if r.Result["status"] != "passed" {
			t.Fatalf("%s: %+v", s.tool, r.Result)
		}
	}
	if r.Result["state"] != "underwriting" {
		t.Fatalf("all checks passed: want underwriting, got %v", r.Result["state"])
	}
	// The verification agent cannot recommend decisions.
	r = e.call(verifier, "decision.recommend", map[string]any{"application_id": appID, "outcome": "approve",
		"reason_codes": []string{"X"}, "narrative": "x"}, vr)
	expect(t, r, "error", "POLICY_DENIED")

	// --- Underwriting: policy, then a recommendation the agent may only propose (L2). ---
	r = e.call(uwAgent, "policy.evaluate", map[string]any{"application_id": appID}, uw)
	expect(t, r, "ok", "")
	if r.Result["outcome"] != "pass" {
		t.Fatalf("policy: %+v", r.Result)
	}
	if dscr := sub(r.Result, "metrics")["dscr"].(float64); dscr < 1.25 {
		t.Fatalf("dscr %v", dscr)
	}
	r = e.call(uwAgent, "decision.recommend", map[string]any{"application_id": appID, "outcome": "approve",
		"terms":        map[string]any{"amount_minor": 5000000, "term_months": 12, "rate_apr_bps": 1900},
		"reason_codes": []string{"STRONG_CASH_FLOW", "CLEAN_BUREAU"}, "confidence": 0.86,
		"narrative": "Four years trading, DSCR above policy, clean bureau and screening.",
		"models":    []map[string]any{{"kind": "rules", "id": "baseline/sme-working-capital", "version": "0.1.0"}}}, uw)
	expect(t, r, "approval_required", "")
	proposalID := r.ApprovalRequestID

	// People find the proposal in their approval queue; agents cannot read the queue.
	r = e.call(officer2, "approval.list", map[string]any{"application_id": appID}, nil)
	expect(t, r, "ok", "")
	queue := r.Result["approval_requests"].([]any)
	if len(queue) != 1 || queue[0].(map[string]any)["approval_request_id"] != proposalID ||
		queue[0].(map[string]any)["you_can_decide"] != true || queue[0].(map[string]any)["tool"] != "decision.recommend" {
		t.Fatalf("approval queue: %+v", queue)
	}
	r = e.call(uwAgent, "approval.list", map[string]any{}, uw)
	expect(t, r, "error", "POLICY_DENIED")

	r = e.call(officer2, "approval.respond", map[string]any{"approval_request_id": proposalID, "decision": "approve",
		"note": "memo reviewed"}, nil)
	expect(t, r, "ok", "")
	recID := sub(r.Result, "result")["decision_record_id"].(string)

	// --- Approval: the limit and maker-checker hold; a senior approver decides. ---
	approve := map[string]any{"application_id": appID, "recommendation_id": recID}
	r = e.call(approver, "decision.approve", approve, nil)
	expect(t, r, "error", "POLICY_DENIED") // 50,000 is above Ben's 25,000 limit
	r = e.call(officer2, "decision.approve", approve, nil)
	expect(t, r, "error", "POLICY_DENIED") // Chai recorded the recommendation
	r = e.call(uwAgent, "decision.approve", approve, uw)
	expect(t, r, "error", "POLICY_DENIED") // agents never decide
	r = e.call(senior, "decision.approve", approve, nil)
	expect(t, r, "ok", "")
	if r.Result["state"] != "decisioned" {
		t.Fatalf("after approval: %+v", r.Result)
	}
	decisionID := r.Result["decision_record_id"].(string)

	// --- Offer: generated and sent by staff, accepted by the borrower. ---
	r = e.call(officer, "offer.generate", map[string]any{"application_id": appID, "decision_record_id": decisionID}, nil)
	expect(t, r, "ok", "")
	offerID := r.Result["offer_id"].(string)
	if p := sub(r.Result, "schedule")["monthly_payment_minor"].(float64); p < 450000 || p > 470000 {
		t.Errorf("monthly payment %v", p)
	}
	r = e.call(borrower, "offer.accept", map[string]any{"offer_id": offerID, "signature": portalClick}, nil)
	expect(t, r, "error", "PRECONDITION_FAILED") // not sent yet
	r = e.call(officer, "offer.send", map[string]any{"offer_id": offerID, "channel": "portal"}, nil)
	expect(t, r, "ok", "")
	if r.Result["state"] != "offered" {
		t.Fatalf("after send: %+v", r.Result)
	}
	r = e.call(borrower, "offer.accept", map[string]any{"offer_id": offerID, "signature": portalClick}, nil)
	expect(t, r, "ok", "")
	if r.Result["state"] != "accepted" {
		t.Fatalf("after accept: %+v", r.Result)
	}

	// --- Hand-off to core banking (operations only). ---
	handoff := map[string]any{"application_id": appID, "target": "mock-core"}
	r = e.call(officer, "loan.handoff", handoff, nil)
	expect(t, r, "error", "POLICY_DENIED")
	r = e.call(ops, "loan.handoff", handoff, nil)
	expect(t, r, "ok", "")
	if r.Result["state"] != "handed_off" || r.Result["external_loan_id"] == "" {
		t.Fatalf("hand-off: %+v", r.Result)
	}

	app = e.state(officer, appID)
	if app["state"] != "handed_off" || app["handoff"] == nil || sub(app, "decision")["actor"] != "user:approver.sara" {
		t.Fatalf("final view: %+v", app)
	}
	if sub(app, "recommendation")["actor"] != "user:officer.chai" {
		t.Errorf("recommendation actor: %+v", app["recommendation"])
	}

	rep, err := gatewayVerify(e)
	if err != nil || !rep {
		t.Fatalf("audit chain invalid (%v)", err)
	}
}

// TestDeclinePath: a confirmed sanctions match refers the case and an approver declines it.
func TestDeclinePath(t *testing.T) {
	e := setup(t)
	vr := map[string]any{"reason": "verification run"}
	uw := map[string]any{"reason": "underwriting analysis"}

	appID, applicant, director := e.newApplication(3000000, "Red River SANCTION Holdings", false)
	// Staff record consent from a signed paper form.
	for _, c := range []struct{ party, scope string }{
		{applicant, "kyc"}, {applicant, "credit_bureau"}, {applicant, "bank_data"}, {director, "kyc"},
	} {
		r := e.call(officer, "consent.record", map[string]any{"application_id": appID, "party_id": c.party, "scope": c.scope,
			"evidence": map[string]any{"method": "signed_form", "document": "consent-form-001"}}, nil)
		expect(t, r, "ok", "")
	}
	r := e.call(verifier, "kyc.run", map[string]any{"application_id": appID, "party_id": applicant}, vr)
	expect(t, r, "ok", "")
	r = e.call(verifier, "sanctions.screen", map[string]any{"application_id": appID, "party_id": applicant}, vr)
	expect(t, r, "ok", "")
	if r.Result["status"] != "needs_review" || r.Result["state"] != "referred" {
		t.Fatalf("sanctions hit should refer the case: %+v", r.Result)
	}

	r = e.call(uwAgent, "policy.evaluate", map[string]any{"application_id": appID, "rule_sets": []string{"compliance"}}, uw)
	expect(t, r, "ok", "")
	if r.Result["outcome"] != "decline" {
		t.Fatalf("policy: %+v", r.Result)
	}
	failed := r.Result["failed"].([]any)
	if failed[0].(map[string]any)["reason_code"] != "SANCTIONS_MATCH" {
		t.Fatalf("failed rules: %+v", failed)
	}
	// Nobody can recommend outside underwriting.
	r = e.call(uwAgent, "decision.recommend", map[string]any{"application_id": appID, "outcome": "approve",
		"reason_codes": []string{"X"}, "narrative": "x"}, uw)
	expect(t, r, "error", "PRECONDITION_FAILED")

	r = e.call(officer, "decision.decline", map[string]any{"application_id": appID, "reason_codes": []string{"SANCTIONS_MATCH"}}, nil)
	expect(t, r, "error", "POLICY_DENIED") // loan officers do not decide
	r = e.call(approver, "decision.decline", map[string]any{"application_id": appID, "reason_codes": []string{"SANCTIONS_MATCH"},
		"note": "confirmed match on consolidated list"}, nil)
	expect(t, r, "ok", "")
	if r.Result["state"] != "closed" {
		t.Fatalf("after decline: %+v", r.Result)
	}
	app := e.state(officer, appID)
	if sub(app, "decision")["outcome"] != "decline" {
		t.Fatalf("decision: %+v", app["decision"])
	}

	var notices int
	ctx := context.Background()
	if err := e.withTenant(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT count(*) FROM events WHERE type = 'AdverseActionNoticeRequired' AND aggregate_id = $1`, appID).Scan(&notices)
	}); err != nil {
		t.Fatal(err)
	}
	if notices != 1 {
		t.Errorf("adverse-action events: %d", notices)
	}
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// withTenant runs fn in a transaction scoped to the demo tenant.
func (e *env) withTenant(ctx context.Context, fn func(querier) error) error {
	return pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('tamarind.tenant_id', $1, true)`, e.g.TenantID("demo")); err != nil {
			return err
		}
		return fn(tx)
	})
}

func gatewayVerify(e *env) (bool, error) {
	rep, err := gateway.VerifyChain(context.Background(), e.pool, e.g.TenantID("demo"))
	return rep.Valid, err
}

func (e *env) feed(token, query string) (int, map[string]any) {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+"/v1/events?"+query, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		e.t.Fatalf("feed: %s", raw)
	}
	return res.StatusCode, out
}

// TestEventFeed: subscribers read events in order, filter by type and long-poll.
func TestEventFeed(t *testing.T) {
	e := setup(t)
	go e.g.Listen(t.Context())

	status, page := e.feed(verifier, "after=0&types=ApplicationStateChanged")
	if status != http.StatusOK || len(page["events"].([]any)) != 0 {
		t.Fatalf("empty feed: %d %+v", status, page)
	}
	if status, _ := e.feed(borrower, "after=0"); status != http.StatusForbidden {
		t.Fatalf("borrowers have no events:read scope, got %d", status)
	}
	if status, _ := e.feed(verifier, "after=-1"); status != http.StatusBadRequest {
		t.Fatalf("bad cursor accepted: %d", status)
	}

	// A long-polling reader wakes up when a submit commits.
	type result struct {
		page    map[string]any
		elapsed time.Duration
	}
	done := make(chan result, 1)
	start := time.Now()
	next := fmt.Sprint(page["next_after"])
	go func() {
		_, p := e.feed(verifier, "after="+next+"&types=ApplicationStateChanged&wait=20")
		done <- result{p, time.Since(start)}
	}()
	time.Sleep(300 * time.Millisecond)
	appID, _, _ := e.newApplication(1000000, "Feed Test Co.", false)
	res := <-done
	events := res.page["events"].([]any)
	if len(events) == 0 || res.elapsed > 10*time.Second {
		t.Fatalf("long poll: %d events after %v", len(events), res.elapsed)
	}
	ev := events[0].(map[string]any)
	if ev["aggregate_id"] != appID || ev["type"] != "ApplicationStateChanged" {
		t.Fatalf("first event: %+v", ev)
	}

	// Paging in order: everything, then nothing new.
	_, all := e.feed(verifier, "after=0&limit=3")
	if len(all["events"].([]any)) != 3 {
		t.Fatalf("limit: %+v", all)
	}
	var last float64
	for _, x := range all["events"].([]any) {
		seq := x.(map[string]any)["seq"].(float64)
		if seq <= last {
			t.Fatal("events out of order")
		}
		last = seq
	}
	_, rest := e.feed(verifier, fmt.Sprintf("after=%v&limit=500", all["next_after"]))
	_, empty := e.feed(verifier, fmt.Sprintf("after=%v", rest["next_after"]))
	if len(empty["events"].([]any)) != 0 {
		t.Fatalf("expected no new events: %+v", empty)
	}
}
