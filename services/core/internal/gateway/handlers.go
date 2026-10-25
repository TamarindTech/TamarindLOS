package gateway

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamarindTech/TamarindLOS/services/core/internal/policy"
)

type handler func(ctx context.Context, tx pgx.Tx, c *call) (any, error)

// registerHandlers lists the tools implemented in this build. Everything else in the
// catalogue returns NOT_IMPLEMENTED.
func (g *Gateway) registerHandlers() map[string]handler {
	return map[string]handler{
		"application.create":       g.applicationCreate,
		"application.get":          g.applicationGet,
		"application.next_actions": g.applicationNextActions,
		"application.submit":       g.applicationSubmit,
		"application.withdraw":     g.applicationWithdraw,
		"party.add":                g.partyAdd,
		"approval.respond":         g.approvalRespond,
		"consent.record":           g.consentRecord,
		"kyc.run":                  g.kycRun,
		"sanctions.screen":         g.sanctionsScreen,
		"credit.pull_report":       g.creditPullReport,
		"bank.analyse_statements":  g.bankAnalyse,
		"policy.evaluate":          g.policyEvaluate,
		"decision.recommend":       g.decisionRecommend,
		"decision.approve":         g.decisionApprove,
		"decision.decline":         g.decisionDecline,
		"offer.generate":           g.offerGenerate,
		"offer.send":               g.offerSend,
		"offer.accept":             g.offerAccept,
		"loan.handoff":             g.loanHandoff,
		"approval.list":            g.approvalList,
		"document.upload_url":      g.documentUploadURL,
		"document.attach":          g.documentAttach,
		"document.classify":        g.documentClassify,
		"document.extract":         g.documentExtract,
		"document.request":         g.documentRequest,
		"evidence.verify":          g.evidenceVerify,
		"task.create":              g.taskCreate,
		"task.complete":            g.taskComplete,
		"task.list":                g.taskList,
		"agent.run_start":          g.agentRunStart,
		"agent.run_finish":         g.agentRunFinish,
		"agent.run_list":           g.agentRunList,
	}
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func num(m map[string]any, k string) (int64, bool) {
	switch v := m[k].(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	}
	return 0, false
}

// application.create
func (g *Gateway) applicationCreate(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	code := str(c.input, "product_code")
	var productID, currency string
	err := tx.QueryRow(ctx, `SELECT id::text, currency FROM products WHERE code = $1 AND status = 'active'
		ORDER BY created_at DESC LIMIT 1`, code).Scan(&productID, &currency)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, newErr("PRODUCT_NOT_FOUND", "no active product with code %q", code)
	}
	if err != nil {
		return nil, err
	}
	if cur := str(c.input, "currency"); cur != currency {
		return nil, newErr(ErrValidation, "product %s is offered in %s, not %s", code, currency, cur)
	}
	amount, _ := num(c.input, "requested_amount_minor")
	term, _ := num(c.input, "requested_term_months")
	channel := str(c.input, "channel")
	if channel == "" {
		channel = map[bool]string{true: "agent", false: "branch"}[c.p.IsAgent()]
	}
	borrower := str(c.input, "borrower")
	if borrower != "" {
		bp, ok := g.Registry.ByID(borrower)
		if !ok || bp.Kind() != "borrower" || bp.Tenant != c.p.Tenant {
			return nil, newErr(ErrValidation, "borrower %q is not a known borrower principal in this tenant", borrower)
		}
	}
	ref := newReference(g.Now())
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO applications
		(id, tenant_id, product_id, reference, state_machine_version, requested_amount_minor, currency,
		 requested_term_months, purpose, channel, created_by, data)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),$10,$11,
		 CASE WHEN $12 = '' THEN '{}'::jsonb ELSE jsonb_build_object('borrower', $12::text) END) RETURNING id::text`,
		newUUID(), c.tenantID, productID, ref, g.Pack.StateMachine.Version, amount, currency, term,
		str(c.input, "purpose"), channel, c.p.ID, borrower).Scan(&id)
	if err != nil {
		return nil, err
	}
	c.app = &appRecord{ID: id, State: "draft"}
	c.addEvent("ApplicationCreated", map[string]any{"reference": ref, "product_code": code,
		"requested_amount_minor": amount, "currency": currency, "requested_term_months": term})
	return map[string]any{"application_id": id, "reference": ref, "state": "draft"}, nil
}

// application.get
func (g *Gateway) applicationGet(_ context.Context, _ pgx.Tx, c *call) (any, error) {
	view := c.app.view()
	// The risk grade drives pricing: staff and agents see it, borrowers see their offer.
	if c.p.Kind() != "borrower" {
		if rg := g.riskGrade(c); rg != nil {
			view["risk_grade"] = riskView(rg, !c.app.allMandatoryChecksDone())
		}
	}
	return map[string]any{"application": view, "state_version": c.app.StateVersion}, nil
}

// application.submit
func (g *Gateway) applicationSubmit(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	if missing := g.missingParties(c.app); len(missing) > 0 {
		return nil, &ToolError{Code: ErrPrecondition, Message: "required parties are missing", Details: map[string]any{"missing_parties": missing}}
	}
	if _, err := tx.Exec(ctx, `UPDATE applications SET submitted_at = now() WHERE id = $1`, c.app.ID); err != nil {
		return nil, err
	}
	c.addEvent("ApplicationSubmitted", map[string]any{})
	return map[string]any{}, nil
}

// application.withdraw
func (g *Gateway) applicationWithdraw(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	reason := str(c.input, "reason")
	if _, err := tx.Exec(ctx, `UPDATE applications SET closed_at = now(),
		data = data || jsonb_build_object('withdrawal_reason', $2::text) WHERE id = $1`, c.app.ID, reason); err != nil {
		return nil, err
	}
	c.addEvent("ApplicationWithdrawn", map[string]any{"reason": reason})
	return map[string]any{}, nil
}

// party.add
func (g *Gateway) partyAdd(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	role, kind := str(c.input, "role"), str(c.input, "kind")
	for _, req := range g.Pack.Parties.Required {
		if req.Role == role && req.Kind != "" && req.Kind != kind {
			return nil, newErr(ErrValidation, "a %s must be a %s for this product", role, req.Kind)
		}
	}
	if role == "applicant" {
		for _, p := range c.app.Parties {
			if p.Role == "applicant" {
				return nil, newErr(ErrValidation, "the application already has an applicant")
			}
		}
	}
	var dob *string
	if s := str(c.input, "date_of_birth"); s != "" {
		if kind != "individual" {
			return nil, newErr(ErrValidation, "date_of_birth applies to individuals only")
		}
		dob = &s
	}
	contact, _ := json.Marshal(c.input["contact"])
	if string(contact) == "null" {
		contact = []byte("{}")
	}
	partyID := newUUID()
	if _, err := tx.Exec(ctx, `INSERT INTO parties (id, tenant_id, kind, legal_name, date_of_birth, registration_number, country, contact)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),$8)`,
		partyID, c.tenantID, kind, str(c.input, "legal_name"), dob, str(c.input, "registration_number"),
		str(c.input, "country"), contact); err != nil {
		return nil, err
	}
	var ownership *float64
	if v, ok := c.input["ownership_pct"].(float64); ok {
		ownership = &v
	}
	if _, err := tx.Exec(ctx, `INSERT INTO application_parties (tenant_id, application_id, party_id, role, ownership_pct)
		VALUES ($1,$2,$3,$4,$5)`, c.tenantID, c.app.ID, partyID, role, ownership); err != nil {
		return nil, err
	}
	c.app.Parties = append(c.app.Parties, partyRecord{PartyID: partyID, Role: role, Kind: kind})
	// Personal data stays out of the event log; events carry identifiers and roles only.
	c.addEvent("PartyAdded", map[string]any{"party_id": partyID, "role": role, "kind": kind})
	return map[string]any{"party_id": partyID}, nil
}

// missingParties checks the pack's party requirements.
func (g *Gateway) missingParties(a *appRecord) []string {
	var missing []string
	for _, req := range g.Pack.Parties.Required {
		n := 0
		for _, p := range a.Parties {
			if p.Role == req.Role {
				n++
			}
		}
		if n < req.Min {
			missing = append(missing, fmt.Sprintf("%s (need %d, have %d)", req.Role, req.Min, n))
		}
	}
	return missing
}

// application.next_actions tells a caller (often an agent) what it can do next.
func (g *Gateway) applicationNextActions(_ context.Context, _ pgx.Tx, c *call) (any, error) {
	type allowed struct {
		Tool     string `json:"tool"`
		Autonomy string `json:"autonomy,omitempty"`
		Outcome  string `json:"outcome"` // execute | approval_required
	}
	type blocked struct {
		Tool  string   `json:"tool"`
		Unmet []string `json:"unmet"`
	}
	var allow []allowed
	var block []blocked
	for _, name := range g.Catalogue.Names {
		t := g.Catalogue.Tools[name]
		if !t.Mutates || !g.Implemented(name) || name == "application.create" || name == "approval.respond" {
			continue
		}
		probe := &call{p: c.p, delegator: c.delegator, tool: t, input: c.input, env: c.env, tenantID: c.tenantID, app: c.app}
		var unmet []string
		if ok, missing := c.p.HasScopes(t.Scopes); !ok {
			unmet = append(unmet, "missing scopes: "+strings.Join(missing, ", "))
		}
		if e := g.authorize(probe, c.p); e != nil {
			unmet = append(unmet, "not permitted by policy")
		}
		if t.Transition != nil && !contains(t.Transition.From, c.app.State) {
			unmet = append(unmet, fmt.Sprintf("state is %s; allowed from %v", c.app.State, t.Transition.From))
		}
		unmet = append(unmet, g.unmetPreconditions(probe)...)
		if name == "application.submit" {
			for _, m := range g.missingParties(c.app) {
				unmet = append(unmet, "missing party: "+m)
			}
		}
		if len(unmet) > 0 {
			block = append(block, blocked{Tool: name, Unmet: unmet})
			continue
		}
		a := allowed{Tool: name, Outcome: "execute"}
		if c.p.IsAgent() {
			a.Autonomy = policy.EffectiveAutonomy(t, g.Pack.Autonomy[name], "", c.p.TrustLevel)
			switch policy.OutcomeFor(a.Autonomy) {
			case policy.AgentApproval:
				a.Outcome = "approval_required"
			case policy.AgentDeny:
				block = append(block, blocked{Tool: name, Unmet: []string{"agents may only assist with this tool"}})
				continue
			}
		}
		allow = append(allow, a)
	}
	if allow == nil {
		allow = []allowed{}
	}
	if block == nil {
		block = []blocked{}
	}
	return map[string]any{
		"state":             c.app.State,
		"state_version":     c.app.StateVersion,
		"completeness_pct":  c.app.CompletenessPct,
		"allowed":           allow,
		"blocked":           block,
		"missing_parties":   g.missingParties(c.app),
		"missing_consents":  nonNil(c.app.MissingConsents),
		"missing_documents": nonNil(c.app.MissingDocuments),
	}, nil
}

// --- approvals --------------------------------------------------------------------

// roleFor decides who must approve a gated call.
func roleFor(tool string) string {
	switch tool {
	case "decision.approve", "decision.decline":
		return "credit_approver"
	case "loan.handoff":
		return "operations"
	default:
		return "loan_officer"
	}
}

func (g *Gateway) requestApproval(ctx context.Context, tx pgx.Tx, c *call, level string) (string, error) {
	input, _ := json.Marshal(c.input)
	onBehalf := ""
	if c.delegator != nil {
		onBehalf = c.delegator.ID
	}
	id := newUUID()
	var appID *string
	if c.app != nil {
		appID = &c.app.ID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO approval_requests
		(id, tenant_id, application_id, tool, tool_input, requested_by, on_behalf_of, required_role, reason, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9,$10)`,
		id, c.tenantID, appID, c.tool.Name, input, c.p.ID, onBehalf, roleFor(c.tool.Name), c.env.Reason,
		g.Now().Add(7*24*time.Hour)); err != nil {
		return "", err
	}
	data := map[string]any{"approval_request_id": id, "tool": c.tool.Name, "autonomy": level, "required_role": roleFor(c.tool.Name)}
	if c.app != nil {
		c.addEvent("ApprovalRequested", data)
	} else {
		c.events = append(c.events, Event{Type: "ApprovalRequested", Data: data,
			AggregateType: "approval_request", AggregateID: id, AggregateVersion: 1})
	}
	return id, g.flushEvents(ctx, tx, c)
}

// approval.respond: a human approves or rejects a gated call. Approving runs the
// original tool as the responder, through the full pipeline (authorization,
// state and preconditions), without the agent autonomy step.
func (g *Gateway) approvalRespond(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	reqID := str(c.input, "approval_request_id")
	var tool, requestedBy, status string
	var onBehalf *string
	var input []byte
	var expires *time.Time
	err := tx.QueryRow(ctx, `SELECT tool, tool_input, requested_by, on_behalf_of, status::text, expires_at
		FROM approval_requests WHERE id = $1 FOR UPDATE`, reqID).Scan(&tool, &input, &requestedBy, &onBehalf, &status, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, newErr(ErrNotFound, "approval request %s not found", reqID)
	}
	if err != nil {
		return nil, err
	}
	if status != "pending" {
		return nil, newErr(ErrStateConflict, "approval request is already %s", status)
	}
	if expires != nil && g.Now().After(*expires) {
		return nil, newErr(ErrStateConflict, "approval request has expired")
	}
	if c.p.ID == requestedBy || (onBehalf != nil && c.p.ID == *onBehalf) {
		return nil, newErr(ErrMakerChecker, "the requester, or the person it acted for, cannot decide this request")
	}
	decision := str(c.input, "decision")
	newStatus := map[string]string{"approve": "approved", "reject": "rejected"}[decision]
	if _, err := tx.Exec(ctx, `UPDATE approval_requests SET status = $2, decided_by = $3, decided_at = now(),
		decision_note = NULLIF($4,'') WHERE id = $1`, reqID, newStatus, c.p.ID, str(c.input, "note")); err != nil {
		return nil, err
	}
	out := map[string]any{"status": newStatus}
	evType := map[string]string{"approve": "ApprovalGranted", "reject": "ApprovalRejected"}[decision]
	evData := map[string]any{"approval_request_id": reqID, "tool": tool, "requested_by": requestedBy}

	if decision == "approve" {
		var orig map[string]any
		if err := json.Unmarshal(input, &orig); err != nil {
			return nil, err
		}
		sub := &call{
			p: c.p, tool: g.Catalogue.Tools[tool], input: orig, tenantID: c.tenantID, proposedBy: requestedBy,
			env: Envelope{IdempotencyKey: "approval:" + reqID, Reason: "approved request " + reqID, TraceID: c.env.TraceID},
		}
		// The approver acts; whoever the proposer represented is still recorded.
		if onBehalf != nil {
			if d, ok := g.Registry.ByID(*onBehalf); ok {
				sub.delegator = d
				sub.env.OnBehalfOf = d.ID
			}
		}
		// Record the decision first, on the application's event stream.
		if c.app != nil {
			c.addEvent(evType, evData)
			if err := g.flushEvents(ctx, tx, c); err != nil {
				return nil, err
			}
		}
		res, err := g.run(ctx, tx, sub, false)
		if err != nil {
			return nil, err
		}
		// Audit the executed call in its own right: performed by the approver.
		if err := g.recordInvocation(ctx, tx, sub, &Response{Status: "ok", Tool: tool, Result: res.Result, InvocationID: newUUID()}, ""); err != nil {
			return nil, err
		}
		out["result"] = res.Result
		if c.app == nil {
			return out, g.approvalEvent(ctx, tx, c, reqID, evType, evData)
		}
		return out, nil
	}
	if c.app != nil {
		c.addEvent(evType, evData)
		return out, nil
	}
	return out, g.approvalEvent(ctx, tx, c, reqID, evType, evData)
}

func (g *Gateway) approvalEvent(ctx context.Context, tx pgx.Tx, c *call, reqID, typ string, data any) error {
	c.events = append(c.events, Event{Type: typ, Data: data, AggregateType: "approval_request", AggregateID: reqID, AggregateVersion: 2})
	return g.flushEvents(ctx, tx, c)
}

// newReference returns a human-friendly application reference, e.g. APP-20261023-7KQ2M.
func newReference(now time.Time) string {
	const alphabet = "ABCDEFGHJKMNPQRSTVWXYZ23456789"
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return "APP-" + now.UTC().Format("20060102") + "-" + string(b)
}
