// Package gateway implements the Tool Gateway: every call from people, systems or
// agents passes through Invoke, which validates, authorizes, checks preconditions and
// autonomy, executes, and records the call (Spec 03 §4).
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamarindTech/TamarindLOS/services/core/internal/auth"
	"github.com/TamarindTech/TamarindLOS/services/core/internal/catalogue"
	"github.com/TamarindTech/TamarindLOS/services/core/internal/connectors"
	"github.com/TamarindTech/TamarindLOS/services/core/internal/policy"
)

// Gateway is the single entry point for tool invocations.
type Gateway struct {
	Catalogue *catalogue.Catalogue
	Pack      *policy.Pack
	Registry  *auth.Registry
	DB        *pgxpool.Pool
	Log       *slog.Logger
	Now       func() time.Time
	// Connectors reach external services. New installs the mock adapters; replace
	// them before serving to use real providers.
	Connectors connectors.Set

	tenantIDs map[string]string // slug -> uuid
	feed      hub
	handlers  map[string]handler
}

// New builds a gateway and resolves tenant ids from the database.
func New(ctx context.Context, cat *catalogue.Catalogue, pack *policy.Pack, reg *auth.Registry, db *pgxpool.Pool, log *slog.Logger) (*Gateway, error) {
	g := &Gateway{Catalogue: cat, Pack: pack, Registry: reg, DB: db, Log: log, Now: time.Now, tenantIDs: map[string]string{}}
	g.handlers = g.registerHandlers()
	g.Connectors = connectors.Mocks(func() time.Time { return g.Now() }, filepath.Join(os.TempDir(), "tamarind-blobs"))
	for slug := range reg.Tenants {
		var id string
		if err := db.QueryRow(ctx, `SELECT id::text FROM tenants WHERE slug = $1`, slug).Scan(&id); err != nil {
			return nil, fmt.Errorf("tenant %q not found in database (run `tamarind migrate --seed`): %w", slug, err)
		}
		g.tenantIDs[slug] = id
	}
	return g, nil
}

// TenantID returns the database id of a tenant slug.
func (g *Gateway) TenantID(slug string) string { return g.tenantIDs[slug] }

// Implemented reports whether a tool has a handler in this build.
func (g *Gateway) Implemented(name string) bool {
	_, ok := g.handlers[name]
	return ok
}

// call is the state of one invocation as it moves through the pipeline.
type call struct {
	p         *auth.Principal
	delegator *auth.Principal
	tool      *catalogue.Tool
	input     map[string]any
	env       Envelope
	tenantID  string
	app       *appRecord // loaded when the input names an application
	events    []Event
	stateFrom string
	// proposedBy is set when a human executes a call an agent proposed (approval.respond).
	proposedBy string
}

// Invoke runs a tool call end to end.
func (g *Gateway) Invoke(ctx context.Context, p *auth.Principal, toolName string, input map[string]any, env Envelope) *Response {
	resp := &Response{Tool: toolName, InvocationID: newUUID()}
	fail := func(e *ToolError) *Response {
		resp.Status, resp.Error = "error", e
		return resp
	}
	tool, ok := g.Catalogue.Tools[toolName]
	if !ok {
		return fail(newErr(ErrToolNotFound, "unknown tool %q", toolName))
	}
	if _, ok := g.handlers[toolName]; !ok {
		return fail(newErr(ErrNotImplemented, "%s is defined in the catalogue but not implemented yet", toolName))
	}
	if input == nil {
		input = map[string]any{}
	}
	c := &call{p: p, tool: tool, input: input, env: env, tenantID: g.tenantIDs[p.Tenant]}

	// Checks that need no database access.
	if e := g.precheck(c); e != nil {
		g.recordFailure(ctx, c, resp, e)
		return fail(e)
	}

	err := withTenantTx(ctx, g.DB, c.tenantID, func(tx pgx.Tx) error {
		// Idempotent replay.
		if env.IdempotencyKey != "" && !env.DryRun {
			if replay, e := g.replay(ctx, tx, c); e != nil || replay != nil {
				if e != nil {
					return e
				}
				*resp = *replay
				resp.Replayed = true
				return errReplayed
			}
		}
		out, e := g.run(ctx, tx, c, true)
		if e != nil {
			return e
		}
		resp.Status, resp.Result, resp.ApprovalRequestID = out.Status, out.Result, out.ApprovalRequestID
		if env.DryRun {
			return errDryRun
		}
		return g.recordInvocation(ctx, tx, c, resp, "")
	})
	switch {
	case err == nil, errors.Is(err, errDryRun), errors.Is(err, errReplayed):
		return resp
	}
	var te *ToolError
	if !errors.As(err, &te) {
		te = g.classify(err)
	}
	g.recordFailure(ctx, c, resp, te)
	return fail(te)
}

var (
	errDryRun   = errors.New("dry run")
	errReplayed = errors.New("replayed")
)

// precheck validates the envelope, scopes, delegation and input schema.
func (g *Gateway) precheck(c *call) *ToolError {
	if c.tenantID == "" {
		return newErr(ErrPolicyDenied, "principal's tenant is not provisioned")
	}
	if c.p.IsAgent() && c.env.Reason == "" {
		return newErr(ErrValidation, "agents must state a reason for every call (envelope.reason)")
	}
	if c.tool.Mutates && c.env.IdempotencyKey == "" && !c.env.DryRun {
		return newErr(ErrValidation, "mutating tools require envelope.idempotency_key")
	}
	if ok, missing := c.p.HasScopes(c.tool.Scopes); !ok {
		return &ToolError{Code: ErrPolicyDenied, Message: "missing scopes", Details: map[string]any{"missing_scopes": missing}}
	}
	if c.env.OnBehalfOf != "" {
		d, ok := g.Registry.ByID(c.env.OnBehalfOf)
		if !ok {
			return newErr(ErrValidation, "on_behalf_of: unknown principal %q", c.env.OnBehalfOf)
		}
		if d.Tenant != c.p.Tenant {
			return newErr(ErrPolicyDenied, "on_behalf_of must be in the same tenant")
		}
		if d.ID == c.p.ID {
			return newErr(ErrValidation, "on_behalf_of cannot be the caller")
		}
		// Delegated scopes are the intersection of both principals' scopes.
		if ok, missing := d.HasScopes(c.tool.Scopes); !ok {
			return &ToolError{Code: ErrPolicyDenied, Message: "the represented principal lacks required scopes", Details: map[string]any{"missing_scopes": missing}}
		}
		c.delegator = d
	}
	if err := c.tool.ValidateInput(c.input); err != nil {
		return &ToolError{Code: ErrValidation, Message: "input does not match the tool schema", Details: err.Error()}
	}
	return nil
}

// runResult is the outcome of the core pipeline.
type runResult struct {
	Status            string
	Result            any
	ApprovalRequestID string
}

// run executes the core pipeline inside a tenant transaction. applyAutonomy is false
// when a human approver executes a previously gated call.
func (g *Gateway) run(ctx context.Context, tx pgx.Tx, c *call, applyAutonomy bool) (*runResult, error) {
	// 1. Load the application the call refers to.
	appID, err := g.applicationIDFor(ctx, tx, c)
	if err != nil {
		return nil, err
	}
	if appID != "" {
		if c.app, err = g.loadApplication(ctx, tx, appID, c.tool.Mutates); err != nil {
			return nil, err
		}
		c.stateFrom = c.app.State
		if v := c.env.ExpectedStateVersion; v != nil && *v != c.app.StateVersion {
			return nil, &ToolError{Code: ErrStateConflict, Message: "application changed since it was read",
				Details: map[string]any{"expected": *v, "actual": c.app.StateVersion}}
		}
	}

	// 2. Authorize the actor and, when delegated, the represented principal (both must pass).
	if e := g.authorize(c, c.p); e != nil {
		return nil, e
	}
	if c.delegator != nil {
		if e := g.authorize(c, c.delegator); e != nil {
			e.Message = "the represented principal is not allowed: " + e.Message
			return nil, e
		}
	}

	// 3. State and preconditions.
	if c.tool.Transition != nil {
		if c.app == nil {
			return nil, newErr(ErrValidation, "%s requires an application_id", c.tool.Name)
		}
		if !contains(c.tool.Transition.From, c.app.State) {
			return nil, &ToolError{Code: ErrPrecondition, Message: fmt.Sprintf("%s is not allowed in state %q", c.tool.Name, c.app.State),
				Details: map[string]any{"allowed_from": c.tool.Transition.From}}
		}
	}
	if unmet := g.unmetPreconditions(c); len(unmet) > 0 {
		return nil, &ToolError{Code: ErrPrecondition, Message: "preconditions not met", Details: map[string]any{"unmet": unmet}}
	}
	if e := g.checkConsent(ctx, tx, c); e != nil {
		return nil, e
	}

	// 4. Autonomy: agents may act, propose for approval, or only assist.
	if applyAutonomy && c.p.IsAgent() {
		level := policy.EffectiveAutonomy(c.tool, g.Pack.Autonomy[c.tool.Name], "", c.p.TrustLevel)
		switch policy.OutcomeFor(level) {
		case policy.AgentDeny:
			return nil, &ToolError{Code: ErrPolicyDenied, Message: fmt.Sprintf("agents may only assist with %s (autonomy %s)", c.tool.Name, level)}
		case policy.AgentApproval:
			if c.env.DryRun {
				return &runResult{Status: "approval_required", Result: map[string]any{"dry_run": true, "autonomy": level}}, nil
			}
			id, err := g.requestApproval(ctx, tx, c, level)
			if err != nil {
				return nil, err
			}
			return &runResult{Status: "approval_required", ApprovalRequestID: id,
				Result: map[string]any{"autonomy": level, "required_role": roleFor(c.tool.Name)}}, nil
		}
	}
	if c.env.DryRun {
		return &runResult{Status: "ok", Result: map[string]any{"dry_run": true, "would": "execute"}}, nil
	}

	// 5. Execute.
	h := g.handlers[c.tool.Name]
	result, err := h(ctx, tx, c)
	if err != nil {
		return nil, err
	}

	// 6. State: the tool's own transition, then a fresh snapshot (completeness, checks,
	// metrics) and any automatic system transitions it unlocks.
	if c.tool.Transition != nil {
		if err := g.transition(ctx, tx, c, c.tool.Transition.To, "tool"); err != nil {
			return nil, err
		}
	}
	if c.app != nil && c.tool.Mutates && c.tool.Name != "approval.respond" {
		if err := g.refresh(ctx, tx, c); err != nil {
			return nil, err
		}
		if err := g.runSystemTransitions(ctx, tx, c); err != nil {
			return nil, err
		}
		if m, ok := result.(map[string]any); ok {
			m["state"] = c.app.State
		}
	}

	// 7. Events.
	if err := g.flushEvents(ctx, tx, c); err != nil {
		return nil, err
	}
	return &runResult{Status: "ok", Result: result}, nil
}

// authorize runs the Cedar check for one subject.
func (g *Gateway) authorize(c *call, p *auth.Principal) *ToolError {
	s := policy.Subject{ID: p.ID, Kind: p.Kind(), TenantID: g.tenantIDs[p.Tenant], Roles: p.Roles,
		ApprovalLimitMinor: g.Pack.ApprovalLimit(p.Roles)}
	r := policy.Resource{ID: "new", TenantID: c.tenantID}
	if c.app != nil {
		r = policy.Resource{ID: c.app.ID, TenantID: c.tenantID, RequestedAmountMinor: c.app.RequestedAmountMinor,
			RecommendedBy: c.app.RecommendedBy, RecommendedFor: c.app.RecommendedFor, Borrower: c.app.Borrower}
	}
	d := g.Pack.Authz.Authorize(s, c.tool.Name, r)
	if !d.Allowed {
		return &ToolError{Code: ErrPolicyDenied, Message: fmt.Sprintf("%s is not permitted to call %s", p.ID, c.tool.Name),
			Details: map[string]any{"policies": d.Reasons, "errors": d.Errors}}
	}
	return nil
}

func (g *Gateway) unmetPreconditions(c *call) []string {
	if len(c.tool.Preconditions) == 0 {
		return nil
	}
	vars := g.celVars(c)
	var unmet []string
	for _, expr := range c.tool.Preconditions {
		ok, err := g.Pack.Exprs.EvalBool(expr, vars)
		if err != nil || !ok {
			unmet = append(unmet, expr)
		}
	}
	return unmet
}

func (g *Gateway) celVars(c *call) map[string]any {
	vars := map[string]any{"product": g.Pack.ProductData, "now": g.Now().UTC()}
	if c.app != nil {
		vars["application"] = c.app.celMap()
		vars["checks"] = c.app.ChecksSummary
		vars["metrics"] = c.app.Metrics
		// The offer named in the input, or else the most recent one.
		var o *offerRecord
		if id, ok := c.input["offer_id"].(string); ok {
			o = c.app.offer(id)
		} else if n := len(c.app.Offers); n > 0 {
			o = &c.app.Offers[n-1]
		}
		if id, ok := c.input["document_id"].(string); ok {
			if d := c.app.document(id); d != nil {
				vars["document"] = map[string]any{"document_id": d.ID, "document_type": d.DocumentType, "status": d.Status}
			}
		}
		if o != nil {
			vars["offer"] = map[string]any{"offer_id": o.ID, "status": o.Status, "expires_at": o.ExpiresAt.UTC(),
				"amount_minor": o.AmountMinor, "term_months": int64(o.TermMonths), "rate_apr_bps": int64(o.RateAPRBps)}
		}
	}
	return vars
}

// refresh reloads the application snapshot after a change and stores derived values.
func (g *Gateway) refresh(ctx context.Context, tx pgx.Tx, c *call) error {
	version := c.app.StateVersion
	app, err := g.loadApplication(ctx, tx, c.app.ID, false)
	if err != nil {
		return err
	}
	app.StateVersion = version
	c.app = app
	_, err = tx.Exec(ctx, `UPDATE applications SET completeness_pct = $2 WHERE id = $1 AND completeness_pct <> $2`,
		app.ID, app.CompletenessPct)
	return err
}

// checkConsent enforces the tool's consent requirements for the party in the input.
func (g *Gateway) checkConsent(ctx context.Context, tx pgx.Tx, c *call) *ToolError {
	if len(c.tool.Consent) == 0 {
		return nil
	}
	partyID, _ := c.input["party_id"].(string)
	for _, scope := range c.tool.Consent {
		var ok bool
		err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM consents WHERE party_id = $1 AND scope = $2
			AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now()))`, partyID, scope).Scan(&ok)
		if err != nil {
			return newErr(ErrInternal, "consent lookup failed")
		}
		if !ok {
			return &ToolError{Code: ErrConsentMissing, Message: fmt.Sprintf("no valid %s consent for party", scope),
				Details: map[string]any{"party_id": partyID, "scope": scope}}
		}
	}
	return nil
}

// transition moves the application to a new state and queues the event.
func (g *Gateway) transition(ctx context.Context, tx pgx.Tx, c *call, to, by string) error {
	from := c.app.State
	if _, err := tx.Exec(ctx, `UPDATE applications SET state = $2, updated_at = now() WHERE id = $1`, c.app.ID, to); err != nil {
		return err
	}
	c.app.State = to
	c.addEvent("ApplicationStateChanged", map[string]any{"from": from, "to": to, "by": by, "tool": c.tool.Name})
	return nil
}

// runSystemTransitions follows automatic transitions whose guards hold.
func (g *Gateway) runSystemTransitions(ctx context.Context, tx pgx.Tx, c *call) error {
	for i := 0; i < 10; i++ { // bounded to avoid cycles
		moved := false
		for _, t := range g.Pack.StateMachine.SystemTransitions(c.app.State) {
			ok, err := g.Pack.Exprs.EvalBool(t.Guard, g.celVars(c))
			if err != nil || !ok {
				continue
			}
			if err := g.transition(ctx, tx, c, t.To, "system"); err != nil {
				return err
			}
			moved = true
			break
		}
		if !moved {
			return nil
		}
	}
	return errors.New("state machine did not settle after 10 automatic transitions")
}

func (c *call) addEvent(typ string, data any) {
	c.events = append(c.events, Event{Type: typ, Data: data})
}

// flushEvents writes queued events. Each event on an application bumps its state_version.
func (g *Gateway) flushEvents(ctx context.Context, tx pgx.Tx, c *call) error {
	onBehalf := ""
	if c.delegator != nil {
		onBehalf = c.delegator.ID
	}
	for _, ev := range c.events {
		ev.TenantID, ev.Actor, ev.OnBehalfOf, ev.TraceID, ev.OccurredAt = c.tenantID, c.p.ID, onBehalf, c.env.TraceID, g.Now()
		if ev.AggregateType == "" {
			if c.app == nil {
				return errors.New("event without aggregate")
			}
			if err := tx.QueryRow(ctx, `UPDATE applications SET state_version = state_version + 1, updated_at = now()
				WHERE id = $1 RETURNING state_version`, c.app.ID).Scan(&c.app.StateVersion); err != nil {
				return err
			}
			ev.AggregateType, ev.AggregateID, ev.AggregateVersion = "application", c.app.ID, c.app.StateVersion
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
	}
	c.events = nil
	return nil
}

// --- Invocation records and idempotency -------------------------------------------

func (g *Gateway) inputHash(c *call) string {
	b, _ := json.Marshal(map[string]any{"input": c.input, "on_behalf_of": c.env.OnBehalfOf})
	return sha256Hex(b)
}

func (g *Gateway) replay(ctx context.Context, tx pgx.Tx, c *call) (*Response, error) {
	var hash string
	var stored []byte
	err := tx.QueryRow(ctx, `SELECT input_sha256, response FROM tool_invocations
		WHERE tenant_id = $1 AND actor = $2 AND tool = $3 AND idempotency_key = $4`,
		c.tenantID, c.p.ID, c.tool.Name, c.env.IdempotencyKey).Scan(&hash, &stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if hash != g.inputHash(c) {
		return nil, newErr(ErrIdempotency, "idempotency key was already used with different input")
	}
	var r Response
	if err := json.Unmarshal(stored, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (g *Gateway) recordInvocation(ctx context.Context, tx pgx.Tx, c *call, resp *Response, errorCode string) error {
	key := c.env.IdempotencyKey
	if key == "" {
		key = "auto:" + resp.InvocationID
	}
	outcome := map[string]string{"ok": "ok", "approval_required": "approval_required", "error": "error"}[resp.Status]
	if errorCode == ErrPolicyDenied || errorCode == ErrMakerChecker {
		outcome = "denied"
	}
	autonomy := "n/a"
	if c.p.IsAgent() {
		autonomy = policy.EffectiveAutonomy(c.tool, g.Pack.Autonomy[c.tool.Name], "", c.p.TrustLevel)
	}
	var appID *string
	if c.app != nil {
		appID = &c.app.ID
	} else if id, ok := c.input["application_id"].(string); ok && isUUID(id) {
		appID = &id
	}
	stored, _ := json.Marshal(redact(resp))
	reason := c.env.Reason
	if reason == "" && c.p.IsAgent() {
		reason = "(no reason given)"
	}
	onBehalf := ""
	if c.delegator != nil {
		onBehalf = c.delegator.ID
	}
	policyResult, _ := json.Marshal(map[string]any{"pack": g.Pack.Name, "pack_version": g.Pack.Version})
	_, err := tx.Exec(ctx, `INSERT INTO tool_invocations
		(id, tenant_id, tool, tool_version, actor, on_behalf_of, application_id, idempotency_key, input_sha256,
		 reason, dry_run, autonomy, policy_result, outcome, error_code, response, trace_id, finished_at)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,$9,NULLIF($10,''),$11,$12,$13,$14,NULLIF($15,''),$16,NULLIF($17,''),now())`,
		resp.InvocationID, c.tenantID, c.tool.Name, c.tool.Version, c.p.ID, onBehalf, appID, key, g.inputHash(c),
		reason, c.env.DryRun, autonomy, policyResult, outcome, errorCode, stored, c.env.TraceID)
	return err
}

// recordFailure stores a failed call (in its own transaction) so that it is audited
// and so a retry with the same idempotency key returns the same error.
func (g *Gateway) recordFailure(ctx context.Context, c *call, resp *Response, e *ToolError) {
	if c.tenantID == "" || e.Code == ErrInternal || e.Code == ErrIdempotency || c.env.DryRun {
		return
	}
	r := *resp
	r.Status, r.Error = "error", e
	err := withTenantTx(ctx, g.DB, c.tenantID, func(tx pgx.Tx) error {
		return g.recordInvocation(ctx, tx, c, &r, e.Code)
	})
	if err != nil {
		g.Log.Warn("could not record failed invocation", "tool", c.tool.Name, "err", err)
	}
}

// classify turns database errors into typed tool errors.
func (g *Gateway) classify(err error) *ToolError {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return newErr(ErrValidation, "conflicts with existing data (%s)", pgErr.ConstraintName)
		case "23514": // check_violation
			if pgErr.ConstraintName == "approval_requests_check" {
				return newErr(ErrMakerChecker, "the requester, or the person it acted for, cannot decide this request")
			}
			return newErr(ErrValidation, "violates a data rule (%s)", pgErr.ConstraintName)
		case "22P02":
			return newErr(ErrValidation, "invalid identifier")
		}
	}
	g.Log.Error("tool call failed", "err", err)
	return newErr(ErrInternal, "internal error")
}

// --- helpers ---------------------------------------------------------------------

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// newUUID returns a UUIDv7 (time-ordered).
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	ms := uint64(time.Now().UnixMilli())
	var t [8]byte
	binary.BigEndian.PutUint64(t[:], ms)
	copy(b[0:6], t[2:8])
	b[6] = (b[6] & 0x0f) | 0x70
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
				return false
			}
		}
	}
	return true
}

// secretResultKeys are result fields that must not be kept in the invocation log
// (they grant access on their own, like upload links).
var secretResultKeys = map[string]bool{"upload_url": true, "upload_token": true}

// redact returns the response with secret result fields replaced.
func redact(resp *Response) *Response {
	m, ok := resp.Result.(map[string]any)
	if !ok {
		return resp
	}
	var copied map[string]any
	for k := range m {
		if secretResultKeys[k] {
			if copied == nil {
				copied = make(map[string]any, len(m))
				for k2, v := range m {
					copied[k2] = v
				}
			}
			copied[k] = "[redacted]"
		}
	}
	if copied == nil {
		return resp
	}
	r := *resp
	r.Result = copied
	return &r
}
