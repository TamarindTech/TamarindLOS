package api_test

// End-to-end tests: real PostgreSQL, real policy pack, calls over REST and MCP.
// Set TAMARIND_TEST_DATABASE_URL to an administrator connection string, e.g.
//   postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable
// The test creates (and drops) its own database.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamarindTech/TamarindLOS/services/core/internal/api"
	"github.com/TamarindTech/TamarindLOS/services/core/internal/auth"
	"github.com/TamarindTech/TamarindLOS/services/core/internal/catalogue"
	"github.com/TamarindTech/TamarindLOS/services/core/internal/gateway"
	"github.com/TamarindTech/TamarindLOS/services/core/internal/policy"
	"github.com/TamarindTech/TamarindLOS/services/core/internal/seed"
	"github.com/TamarindTech/TamarindLOS/services/core/migrations"
)

const root = "../../../.."

type env struct {
	t       *testing.T
	adminDB string // administrator URL for the test database
	srv     *httptest.Server
	g       *gateway.Gateway
	pool    *pgxpool.Pool
	n       int
}

func setup(t *testing.T) *env {
	t.Helper()
	adminURL := os.Getenv("TAMARIND_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("TAMARIND_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	dbName := fmt.Sprintf("tamarind_e2e_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
		admin.Close(context.Background())
	})

	u, _ := url.Parse(adminURL)
	u.Path = "/" + dbName
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrations.Apply(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "ALTER ROLE tamarind_app PASSWORD 'e2e-app-password'"); err != nil {
		t.Fatal(err)
	}
	cat, err := catalogue.Load(root + "/tools")
	if err != nil {
		t.Fatal(err)
	}
	pack, err := policy.LoadPack(root + "/policy-packs/baseline/sme-working-capital")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := auth.LoadRegistry(root + "/deploy/compose/principals.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Run(ctx, conn, reg, pack); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)

	u.User = url.UserPassword("tamarind_app", "e2e-app-password")
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	g, err := gateway.New(ctx, cat, pack, reg, pool, log)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&api.Server{G: g, Log: log}).Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, g: g, pool: pool, adminDB: adminDBURL(adminURL, dbName)}
}

type resp struct {
	HTTP              int
	Status            string         `json:"status"`
	Result            map[string]any `json:"result"`
	ApprovalRequestID string         `json:"approval_request_id"`
	Replayed          bool           `json:"replayed"`
	Error             *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details any    `json:"details"`
	} `json:"error"`
}

func (r resp) code() string {
	if r.Error == nil {
		return ""
	}
	return r.Error.Code
}

// call invokes a tool over REST. envelope may be nil; a fresh idempotency key is added.
func (e *env) call(token, tool string, input map[string]any, envelope map[string]any) resp {
	e.t.Helper()
	e.n++
	body := map[string]any{"input": input, "idempotency_key": fmt.Sprintf("k-%d", e.n)}
	for k, v := range envelope {
		body[k] = v
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/tools/"+tool, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	var out resp
	raw, _ := io.ReadAll(res.Body)
	if err := json.Unmarshal(raw, &out); err != nil {
		e.t.Fatalf("%s: bad response %s", tool, raw)
	}
	out.HTTP = res.StatusCode
	return out
}

func (e *env) mcp(token, method string, params any) map[string]any {
	e.t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest("POST", e.srv.URL+"/mcp", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		e.t.Fatal(err)
	}
	return out
}

const (
	officer  = "dev-officer-ana"
	officer2 = "dev-officer-chai"
	approver = "dev-approver-ben"
	agent    = "dev-agent-intake"
	uwAgent  = "dev-agent-underwriting"
	foreign  = "dev-other-zoe"
)

func expect(t *testing.T, r resp, status, code string) {
	t.Helper()
	if r.Status != status || r.code() != code {
		t.Fatalf("got status=%s code=%s (http %d, %+v); want status=%s code=%s", r.Status, r.code(), r.HTTP, r.Error, status, code)
	}
}

func TestEndToEnd(t *testing.T) {
	e := setup(t)
	reason := map[string]any{"reason": "processing new SME application"}

	// --- An agent creates an application and adds parties. ---
	r := e.call(agent, "application.create", map[string]any{
		"product_code": "SME-WC", "requested_amount_minor": 3000000, "currency": "USD", "requested_term_months": 12,
		"purpose": "inventory for peak season"}, nil)
	expect(t, r, "error", "VALIDATION_FAILED") // agents must give a reason

	r = e.call(agent, "application.create", map[string]any{
		"product_code": "SME-WC", "requested_amount_minor": 3000000, "currency": "USD", "requested_term_months": 12,
		"purpose": "inventory for peak season"}, reason)
	expect(t, r, "ok", "")
	appID := r.Result["application_id"].(string)
	if !strings.HasPrefix(r.Result["reference"].(string), "APP-") {
		t.Errorf("unexpected reference %v", r.Result["reference"])
	}

	// Schema validation.
	r = e.call(agent, "party.add", map[string]any{"application_id": appID, "kind": "robot", "role": "applicant", "legal_name": "X"}, reason)
	expect(t, r, "error", "VALIDATION_FAILED")
	// Pack rule: the applicant must be a business.
	r = e.call(agent, "party.add", map[string]any{"application_id": appID, "kind": "individual", "role": "applicant", "legal_name": "X"}, reason)
	expect(t, r, "error", "VALIDATION_FAILED")

	// Submitting without parties fails a CEL precondition.
	r = e.call(agent, "application.submit", map[string]any{"application_id": appID}, reason)
	expect(t, r, "error", "PRECONDITION_FAILED")

	r = e.call(agent, "party.add", map[string]any{"application_id": appID, "kind": "business", "role": "applicant",
		"legal_name": "Mekong Trading Co.", "registration_number": "0105561234567", "country": "TH"}, reason)
	expect(t, r, "ok", "")

	// Submitting with an applicant but no director fails the pack's party requirement.
	r = e.call(agent, "application.submit", map[string]any{"application_id": appID}, reason)
	expect(t, r, "error", "PRECONDITION_FAILED")

	r = e.call(agent, "party.add", map[string]any{"application_id": appID, "kind": "individual", "role": "director",
		"legal_name": "Somchai P.", "date_of_birth": "1980-04-02", "ownership_pct": 60}, reason)
	expect(t, r, "ok", "")

	// --- next_actions tells the agent what it may do. ---
	r = e.call(agent, "application.next_actions", map[string]any{"application_id": appID}, reason)
	expect(t, r, "ok", "")
	outcomes := map[string]string{}
	for _, a := range r.Result["allowed"].([]any) {
		m := a.(map[string]any)
		outcomes[m["tool"].(string)] = m["outcome"].(string)
	}
	if outcomes["application.submit"] != "execute" {
		t.Errorf("submit should be executable by the agent, got %v", outcomes)
	}
	if outcomes["application.withdraw"] != "approval_required" {
		t.Errorf("withdraw should require approval for the agent, got %v", outcomes)
	}

	// --- Optimistic concurrency. ---
	stale := 1
	r = e.call(agent, "application.submit", map[string]any{"application_id": appID},
		map[string]any{"reason": "submit", "expected_state_version": stale})
	expect(t, r, "error", "STATE_CONFLICT")

	// --- Submit (agent, L3) moves draft -> submitted -> intake automatically. ---
	submitEnv := map[string]any{"reason": "all required parties present", "idempotency_key": "submit-1"}
	r = e.call(agent, "application.submit", map[string]any{"application_id": appID}, submitEnv)
	expect(t, r, "ok", "")
	if r.Result["state"] != "intake" {
		t.Fatalf("expected state intake, got %v", r.Result["state"])
	}
	// Idempotent replay returns the original result.
	r = e.call(agent, "application.submit", map[string]any{"application_id": appID}, submitEnv)
	expect(t, r, "ok", "")
	if !r.Replayed {
		t.Error("expected replayed response")
	}
	// Same key, different input.
	r = e.call(agent, "application.submit", map[string]any{"application_id": "00000000-0000-7000-8000-000000000000"}, submitEnv)
	expect(t, r, "error", "IDEMPOTENCY_CONFLICT")

	// --- Agents can never respond to approval requests (no scope, and Cedar forbids it). ---
	r = e.call(agent, "approval.respond", map[string]any{"approval_request_id": appID, "decision": "approve"}, reason)
	expect(t, r, "error", "POLICY_DENIED")

	// --- Autonomy: the agent asks to withdraw on behalf of Ana; that needs a human. ---
	r = e.call(agent, "application.withdraw", map[string]any{"application_id": appID, "reason": "customer cancelled by phone"},
		map[string]any{"reason": "customer asked to cancel", "on_behalf_of": "user:officer.ana"})
	expect(t, r, "approval_required", "")
	if r.HTTP != http.StatusAccepted {
		t.Errorf("approval_required should be HTTP 202, got %d", r.HTTP)
	}
	reqID := r.ApprovalRequestID

	// Maker-checker: Ana (the represented user) cannot approve her own request.
	r = e.call(officer, "approval.respond", map[string]any{"approval_request_id": reqID, "decision": "approve"}, nil)
	expect(t, r, "error", "MAKER_CHECKER_VIOLATION")

	// Ben is a credit approver: he may respond, but he is not allowed to withdraw
	// applications himself, so executing the approved call is denied and nothing changes.
	r = e.call(approver, "approval.respond", map[string]any{"approval_request_id": reqID, "decision": "approve"}, nil)
	expect(t, r, "error", "POLICY_DENIED")

	// Chai, another loan officer, approves; the withdrawal executes as Chai.
	r = e.call(officer2, "approval.respond", map[string]any{"approval_request_id": reqID, "decision": "approve", "note": "confirmed with customer"}, nil)
	expect(t, r, "ok", "")
	if res, _ := r.Result["result"].(map[string]any); res["state"] != "withdrawn" {
		t.Fatalf("expected withdrawn, got %+v", r.Result)
	}
	// The request cannot be decided twice.
	r = e.call(officer2, "approval.respond", map[string]any{"approval_request_id": reqID, "decision": "reject"}, nil)
	expect(t, r, "error", "STATE_CONFLICT")

	// --- Tenant isolation: another lender cannot see the application. ---
	r = e.call(foreign, "application.get", map[string]any{"application_id": appID}, nil)
	expect(t, r, "error", "NOT_FOUND")

	r = e.call(officer, "application.get", map[string]any{"application_id": appID}, nil)
	expect(t, r, "ok", "")
	app := r.Result["application"].(map[string]any)
	if app["state"] != "withdrawn" || len(app["parties"].([]any)) != 2 {
		t.Errorf("unexpected application view: %+v", app)
	}

	// --- Unimplemented tools are explicit. ---
	r = e.call(officer, "kyc.run", map[string]any{"application_id": appID, "party_id": appID}, nil)
	expect(t, r, "error", "NOT_IMPLEMENTED")

	// --- MCP. ---
	init := e.mcp(agent, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	if init["result"].(map[string]any)["protocolVersion"] != "2025-06-18" {
		t.Errorf("initialize: %+v", init)
	}
	list := e.mcp(agent, "tools/list", map[string]any{})
	names := []string{}
	for _, tl := range list["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "application_create") || strings.Contains(joined, "approval_respond") || strings.Contains(joined, "kyc_run") {
		t.Errorf("tools/list for the intake agent: %s", joined)
	}
	created := e.mcp(agent, "tools/call", map[string]any{"name": "application_create", "arguments": map[string]any{
		"product_code": "SME-WC", "requested_amount_minor": 1500000, "currency": "USD", "requested_term_months": 6,
		"_envelope": map[string]any{"reason": "new enquiry from web chat", "idempotency_key": "mcp-1"}}})
	result := created["result"].(map[string]any)
	if result["isError"] != false {
		t.Fatalf("MCP create failed: %+v", result)
	}
	sc := result["structuredContent"].(map[string]any)
	if sc["status"] != "ok" {
		t.Fatalf("MCP create: %+v", sc)
	}

	// --- The audit chain verifies. ---
	rep, err := gateway.VerifyChain(context.Background(), e.pool, e.g.TenantID("demo"))
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Valid || rep.Events < 10 {
		t.Errorf("audit chain: %+v", rep)
	}

	// --- Every call was recorded, including denials. ---
	var denied, agentCalls int
	err = pgx.BeginFunc(context.Background(), e.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(), `SELECT set_config('tamarind.tenant_id', $1, true)`, e.g.TenantID("demo")); err != nil {
			return err
		}
		return tx.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE outcome = 'denied'),
			count(*) FILTER (WHERE actor LIKE 'agent:%') FROM tool_invocations`).Scan(&denied, &agentCalls)
	})
	if err != nil {
		t.Fatal(err)
	}
	if denied < 2 || agentCalls < 8 {
		t.Errorf("invocation log: denied=%d agent calls=%d", denied, agentCalls)
	}
}

func adminDBURL(adminURL, db string) string {
	u, _ := url.Parse(adminURL)
	u.Path = "/" + db
	return u.String()
}

func TestAuditChainDetectsTampering(t *testing.T) {
	e := setup(t)
	r := e.call(officer, "application.create", map[string]any{
		"product_code": "SME-WC", "requested_amount_minor": 900000, "currency": "USD", "requested_term_months": 9}, nil)
	expect(t, r, "ok", "")
	ctx := context.Background()
	if rep, _ := gateway.VerifyChain(ctx, e.pool, e.g.TenantID("demo")); !rep.Valid {
		t.Fatalf("chain should be valid before tampering: %+v", rep)
	}
	// A database administrator bypasses the append-only trigger and edits history.
	admin, err := pgx.Connect(ctx, e.adminDB)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, `ALTER TABLE tamarind.events DISABLE TRIGGER events_append_only;
		UPDATE tamarind.events SET data = jsonb_set(data, '{requested_amount_minor}', '1') WHERE type = 'ApplicationCreated';
		ALTER TABLE tamarind.events ENABLE TRIGGER events_append_only;`); err != nil {
		t.Fatal(err)
	}
	rep, err := gateway.VerifyChain(ctx, e.pool, e.g.TenantID("demo"))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Valid {
		t.Fatal("tampering was not detected")
	}
	// And the application role cannot edit events at all.
	err = pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE events SET type = 'X'`)
		return err
	})
	if err == nil {
		t.Fatal("application role was able to update events")
	}
}

func TestStartPage(t *testing.T) {
	e := setup(t)
	res, err := http.Get(e.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "Tamarind LOS") ||
		!strings.Contains(string(body), "application_create") || !strings.Contains(string(body), "dev-agent-intake") {
		t.Errorf("start page: status %d", res.StatusCode)
	}
	res2, err := http.Get(e.srv.URL + "/nope")
	if err != nil {
		t.Fatal(err)
	}
	res2.Body.Close()
	if res2.StatusCode != http.StatusNotFound {
		t.Errorf("unknown path should be 404, got %d", res2.StatusCode)
	}
}

func TestUnauthenticated(t *testing.T) {
	e := setup(t)
	res, err := http.Post(e.srv.URL+"/v1/tools/application.get", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("got %d", res.StatusCode)
	}
}
