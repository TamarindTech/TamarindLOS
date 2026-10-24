package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamarindTech/TamarindLOS/services/core/internal/connectors"
	"github.com/TamarindTech/TamarindLOS/services/core/internal/policy"
)

// Error codes specific to the workflow tools.
const (
	ErrProviderUnavailable = "PROVIDER_UNAVAILABLE"
	ErrTargetUnavailable   = "TARGET_UNAVAILABLE"
)

func (p partyRecord) connectorParty() connectors.Party {
	return connectors.Party{ID: p.PartyID, Kind: p.Kind, Role: p.Role, LegalName: p.LegalName,
		RegistrationNumber: p.RegistrationNumber, Country: p.Country, DateOfBirth: p.DateOfBirth}
}

// inputParty returns the application party named by input.party_id.
func (c *call) inputParty() (*partyRecord, error) {
	id := str(c.input, "party_id")
	p := c.app.party(id)
	if p == nil {
		return nil, newErr(ErrNotFound, "party %s is not on this application", id)
	}
	return p, nil
}

// --- consent ------------------------------------------------------------------------

// consent.record
func (g *Gateway) consentRecord(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	partyID := str(c.input, "party_id")
	if c.app != nil {
		if _, err := c.inputParty(); err != nil {
			return nil, err
		}
	} else {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM parties WHERE id = $1)`, partyID).Scan(&ok); err != nil {
			return nil, err
		}
		if !ok {
			return nil, newErr(ErrNotFound, "party %s not found", partyID)
		}
	}
	var expires *time.Time
	if s := str(c.input, "expires_at"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return nil, newErr(ErrValidation, "expires_at must be an RFC 3339 timestamp")
		}
		if !t.After(g.Now()) {
			return nil, newErr(ErrValidation, "expires_at must be in the future")
		}
		expires = &t
	}
	evidence, _ := c.input["evidence"].(map[string]any)
	ev := map[string]any{"captured": evidence, "recorded_by": c.p.ID, "recorded_at": g.Now().UTC().Format(time.RFC3339)}
	evJSON, _ := json.Marshal(ev)
	var appID *string
	if c.app != nil {
		appID = &c.app.ID
	}
	id := newUUID()
	scope := str(c.input, "scope")
	if _, err := tx.Exec(ctx, `INSERT INTO consents (id, tenant_id, party_id, application_id, scope, evidence, expires_at, recorded_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, id, c.tenantID, partyID, appID, scope, evJSON, expires, c.p.ID); err != nil {
		return nil, err
	}
	data := map[string]any{"consent_id": id, "party_id": partyID, "scope": scope}
	if c.app != nil {
		c.addEvent("ConsentRecorded", data)
	} else {
		c.events = append(c.events, Event{Type: "ConsentRecorded", Data: data, AggregateType: "consent", AggregateID: id, AggregateVersion: 1})
	}
	return map[string]any{"consent_id": id}, nil
}

// --- checks -------------------------------------------------------------------------

// saveCheck stores a connector result as a check and queues CheckCompleted.
func (g *Gateway) saveCheck(ctx context.Context, tx pgx.Tx, c *call, p *partyRecord, checkType, consentScope string,
	r connectors.CheckResult, err error) (map[string]any, error) {
	if err != nil {
		g.Log.Warn("connector failed", "tool", c.tool.Name, "err", err)
		return nil, newErr(ErrProviderUnavailable, "%s provider is unavailable", checkType)
	}
	var consentID *string
	if consentScope != "" {
		for _, cr := range c.app.Consents {
			if cr.PartyID == p.PartyID && cr.Scope == consentScope {
				id := cr.ID
				consentID = &id
			}
		}
	}
	result, _ := json.Marshal(r.Result)
	id := newUUID()
	if _, err := tx.Exec(ctx, `INSERT INTO checks (id, tenant_id, application_id, party_id, check_type, provider, status,
		result, external_ref, consent_id, requested_by, completed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),$10,$11,now())`,
		id, c.tenantID, c.app.ID, p.PartyID, checkType, r.Provider, r.Status, result, r.ExternalRef, consentID, c.p.ID); err != nil {
		return nil, err
	}
	c.addEvent("CheckCompleted", map[string]any{"check_id": id, "check_type": checkType, "party_id": p.PartyID,
		"status": r.Status, "provider": r.Provider})
	return map[string]any{"check_id": id, "check_type": checkType, "status": r.Status}, nil
}

// kyc.run: KYC for individuals, KYB for businesses.
func (g *Gateway) kycRun(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	p, err := c.inputParty()
	if err != nil {
		return nil, err
	}
	if p.Kind == "business" {
		r, err := g.Connectors.Identity.VerifyBusiness(ctx, p.connectorParty())
		return g.saveCheck(ctx, tx, c, p, "kyb", "kyc", r, err)
	}
	level := str(c.input, "level")
	if level == "" {
		level = "standard"
	}
	r, err := g.Connectors.Identity.VerifyIndividual(ctx, p.connectorParty(), level)
	return g.saveCheck(ctx, tx, c, p, "kyc", "kyc", r, err)
}

// sanctions.screen
func (g *Gateway) sanctionsScreen(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	p, err := c.inputParty()
	if err != nil {
		return nil, err
	}
	r, err := g.Connectors.Sanctions.Screen(ctx, p.connectorParty())
	out, err := g.saveCheck(ctx, tx, c, p, "sanctions", "", r, err)
	if out != nil {
		out["potential_matches"] = r.Result["potential_matches"]
	}
	return out, err
}

// credit.pull_report
func (g *Gateway) creditPullReport(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	p, err := c.inputParty()
	if err != nil {
		return nil, err
	}
	report := str(c.input, "report_type")
	if report == "consumer" && p.Kind != "individual" {
		return nil, newErr(ErrValidation, "consumer reports apply to individuals only")
	}
	if report == "commercial" && p.Kind != "business" {
		return nil, newErr(ErrValidation, "commercial reports apply to businesses only")
	}
	r, err := g.Connectors.Bureau.Report(ctx, p.connectorParty(), report)
	return g.saveCheck(ctx, tx, c, p, "credit_bureau", "credit_bureau", r, err)
}

// bank.analyse_statements
func (g *Gateway) bankAnalyse(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	p, err := c.inputParty()
	if err != nil {
		return nil, err
	}
	if p.Kind != "business" {
		return nil, newErr(ErrValidation, "bank analysis runs on the business applicant")
	}
	r, err := g.Connectors.Bank.Analyse(ctx, p.connectorParty(), str(c.input, "source"), c.app.RequestedAmountMinor)
	out, err := g.saveCheck(ctx, tx, c, p, "bank_analysis", "bank_data", r, err)
	if out != nil {
		out["months_covered"] = r.Result["months_covered"]
		out["metrics"] = r.Result
	}
	return out, err
}

// --- policy and decisions -------------------------------------------------------

// evaluate runs the pack's rules, optionally limited to some rule sets.
func (g *Gateway) evaluate(c *call, sets []string) policy.Evaluation {
	ev := g.Pack.EvaluateRules(g.celVars(c))
	if len(sets) == 0 {
		return ev
	}
	out := policy.Evaluation{Outcome: "pass"}
	for _, r := range ev.Results {
		if !contains(sets, r.Set) {
			continue
		}
		out.Results = append(out.Results, r)
		if !r.Passed {
			if r.Outcome == "decline" {
				out.Outcome = "decline"
			} else if out.Outcome == "pass" {
				out.Outcome = "refer"
			}
		}
	}
	return out
}

func failedReasons(ev policy.Evaluation) []map[string]string {
	out := []map[string]string{}
	for _, r := range ev.Results {
		if !r.Passed {
			out = append(out, map[string]string{"rule_id": r.RuleID, "outcome": r.Outcome, "reason_code": r.ReasonCode})
		}
	}
	return out
}

// policy.evaluate
func (g *Gateway) policyEvaluate(_ context.Context, _ pgx.Tx, c *call) (any, error) {
	var sets []string
	if list, ok := c.input["rule_sets"].([]any); ok {
		for _, s := range list {
			sets = append(sets, fmt.Sprint(s))
		}
	}
	ev := g.evaluate(c, sets)
	return map[string]any{"pack": g.Pack.Name, "pack_version": g.Pack.Version, "outcome": ev.Outcome,
		"results": ev.Results, "failed": failedReasons(ev), "metrics": c.app.Metrics}, nil
}

// terms reads amount, term and rate from a terms object.
type terms struct {
	AmountMinor int64 `json:"amount_minor"`
	TermMonths  int64 `json:"term_months"`
	RateAPRBps  int64 `json:"rate_apr_bps"`
}

func termsFrom(m map[string]any, base terms) terms {
	if v, ok := num(m, "amount_minor"); ok {
		base.AmountMinor = v
	}
	if v, ok := num(m, "term_months"); ok {
		base.TermMonths = v
	}
	if v, ok := num(m, "rate_apr_bps"); ok {
		base.RateAPRBps = v
	}
	return base
}

func (t terms) asMap() map[string]any {
	return map[string]any{"amount_minor": t.AmountMinor, "term_months": t.TermMonths, "rate_apr_bps": t.RateAPRBps}
}

// validTerms checks terms against the product and the request.
func (g *Gateway) validTerms(a *appRecord, t terms) *ToolError {
	pr := g.Pack.Product
	switch {
	case t.AmountMinor < pr.AmountMinor.Min || t.AmountMinor > pr.AmountMinor.Max:
		return newErr(ErrValidation, "amount %d is outside the product range", t.AmountMinor)
	case t.AmountMinor > a.RequestedAmountMinor:
		return newErr(ErrValidation, "terms cannot exceed the requested amount (%d)", a.RequestedAmountMinor)
	case t.TermMonths < pr.TermMonths.Min || t.TermMonths > pr.TermMonths.Max:
		return newErr(ErrValidation, "term %d months is outside the product range", t.TermMonths)
	case t.RateAPRBps <= 0:
		return newErr(ErrValidation, "rate_apr_bps must be positive")
	}
	return nil
}

func (g *Gateway) insertDecision(ctx context.Context, tx pgx.Tx, c *call, kind, outcome string, t *terms, conditions any,
	reasons []string, inputs map[string]any, ev policy.Evaluation, narrative string, confidence *float64, basedOn string) (string, error) {
	var termsJSON []byte
	if t != nil {
		termsJSON, _ = json.Marshal(t.asMap())
	}
	if conditions == nil {
		conditions = []any{}
	}
	if reasons == nil {
		reasons = []string{}
	}
	condJSON, _ := json.Marshal(conditions)
	inJSON, _ := json.Marshal(inputs)
	polJSON, _ := json.Marshal(map[string]any{"pack": g.Pack.Name, "pack_version": g.Pack.Version, "outcome": ev.Outcome, "results": ev.Results})
	onBehalf := ""
	if c.delegator != nil {
		onBehalf = c.delegator.ID
	}
	id := newUUID()
	_, err := tx.Exec(ctx, `INSERT INTO decision_records (id, tenant_id, application_id, kind, outcome, terms, conditions,
		reason_codes, inputs, policy, narrative, confidence, actor, on_behalf_of, based_on)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULLIF($11,''),$12,$13,NULLIF($14,''),NULLIF($15,'')::uuid)`,
		id, c.tenantID, c.app.ID, kind, outcome, termsJSON, condJSON, reasons, inJSON, polJSON, narrative, confidence,
		c.p.ID, onBehalf, basedOn)
	return id, err
}

func stringList(v any) []string {
	var out []string
	if list, ok := v.([]any); ok {
		for _, s := range list {
			out = append(out, fmt.Sprint(s))
		}
	}
	return out
}

// decision.recommend stores an immutable recommendation for an approver.
func (g *Gateway) decisionRecommend(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	outcome := str(c.input, "outcome")
	ev := g.evaluate(c, nil)
	approving := outcome == "approve" || outcome == "approve_with_conditions"
	if approving && ev.Outcome == "decline" {
		return nil, &ToolError{Code: ErrPrecondition, Message: "policy declines this application; it cannot be recommended for approval",
			Details: map[string]any{"failed": failedReasons(ev)}}
	}
	if outcome == "approve_with_conditions" && len(stringList(c.input["conditions"])) == 0 {
		return nil, newErr(ErrValidation, "approve_with_conditions needs at least one condition")
	}
	var t *terms
	if approving {
		tt := termsFrom(mapOf(c.input["terms"]), terms{AmountMinor: c.app.RequestedAmountMinor,
			TermMonths: c.app.RequestedTermMonths, RateAPRBps: int64(g.Pack.Product.ReferenceAPRBps())})
		if e := g.validTerms(c.app, tt); e != nil {
			return nil, e
		}
		t = &tt
	}
	var confidence *float64
	if v, ok := c.input["confidence"].(float64); ok {
		confidence = &v
	}
	proposedBy := c.p.ID
	if c.proposedBy != "" {
		proposedBy = c.proposedBy
	}
	inputs := map[string]any{"checks": c.app.ChecksSummary, "metrics": c.app.Metrics, "proposed_by": proposedBy}
	id, err := g.insertDecision(ctx, tx, c, "recommendation", outcome, t, stringList(c.input["conditions"]),
		stringList(c.input["reason_codes"]), inputs, ev, str(c.input, "narrative"), confidence, "")
	if err != nil {
		return nil, err
	}
	c.addEvent("DecisionRecommended", map[string]any{"decision_record_id": id, "outcome": outcome,
		"policy_outcome": ev.Outcome, "proposed_by": proposedBy})
	return map[string]any{"decision_record_id": id, "outcome": outcome, "policy_outcome": ev.Outcome}, nil
}

func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// decision.approve: a human approves on the recommended (or amended) terms.
func (g *Gateway) decisionApprove(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	rec := c.app.Recommendation
	if rec == nil || rec.ID != str(c.input, "recommendation_id") {
		return nil, newErr(ErrPrecondition, "recommendation_id must be the application's latest recommendation")
	}
	if rec.Outcome != "approve" && rec.Outcome != "approve_with_conditions" {
		return nil, newErr(ErrPrecondition, "the latest recommendation is %q, not an approval", rec.Outcome)
	}
	ev := g.evaluate(c, nil)
	if ev.Outcome == "decline" {
		return nil, &ToolError{Code: ErrPrecondition, Message: "policy declines this application",
			Details: map[string]any{"failed": failedReasons(ev)}}
	}
	t := termsFrom(mapOf(c.input["amended_terms"]), termsFrom(rec.Terms, terms{}))
	if e := g.validTerms(c.app, t); e != nil {
		return nil, e
	}
	outcome := rec.Outcome
	inputs := map[string]any{"note": str(c.input, "note"), "amended": c.input["amended_terms"] != nil}
	id, err := g.insertDecision(ctx, tx, c, "decision", outcome, &t, rec.Conditions, rec.ReasonCodes, inputs, ev, "", nil, rec.ID)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE applications SET decision_outcome = $2 WHERE id = $1`, c.app.ID, outcome); err != nil {
		return nil, err
	}
	c.addEvent("ApplicationApproved", map[string]any{"decision_record_id": id, "recommendation_id": rec.ID,
		"outcome": outcome, "terms": t.asMap()})
	return map[string]any{"decision_record_id": id}, nil
}

// decision.decline: a human declines; reason codes drive the adverse-action notice.
func (g *Gateway) decisionDecline(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	basedOn := str(c.input, "recommendation_id")
	if basedOn != "" && (c.app.Recommendation == nil || c.app.Recommendation.ID != basedOn) {
		return nil, newErr(ErrPrecondition, "recommendation_id must be the application's latest recommendation")
	}
	reasons := stringList(c.input["reason_codes"])
	ev := g.evaluate(c, nil)
	id, err := g.insertDecision(ctx, tx, c, "decision", "decline", nil, nil, reasons,
		map[string]any{"note": str(c.input, "note")}, ev, "", nil, basedOn)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE applications SET decision_outcome = 'decline', closed_at = now() WHERE id = $1`, c.app.ID); err != nil {
		return nil, err
	}
	c.addEvent("ApplicationDeclined", map[string]any{"decision_record_id": id, "reason_codes": reasons})
	c.addEvent("AdverseActionNoticeRequired", map[string]any{"decision_record_id": id, "reason_codes": reasons})
	return map[string]any{"decision_record_id": id}, nil
}

// --- offers and hand-off -------------------------------------------------------

// offer.generate builds an offer from the approved decision.
func (g *Gateway) offerGenerate(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	d := c.app.Decision
	if d == nil || d.ID != str(c.input, "decision_record_id") {
		return nil, newErr(ErrPrecondition, "decision_record_id must be the application's final decision")
	}
	for _, o := range c.app.Offers {
		if o.Status == "draft" || o.Status == "sent" {
			return nil, newErr(ErrStateConflict, "offer %s is already %s", o.ID, o.Status)
		}
	}
	t := termsFrom(d.Terms, terms{})
	days, ok := num(c.input, "validity_days")
	if !ok {
		days = int64(g.Pack.Product.OfferValidityDays)
	}
	if days <= 0 {
		days = 14
	}
	payment := monthlyPayment(t.AmountMinor, int(t.TermMonths), int(t.RateAPRBps))
	total := payment * t.TermMonths
	schedule := map[string]any{"repayment": g.Pack.Product.Repayment, "payments": t.TermMonths,
		"monthly_payment_minor": payment, "total_repayable_minor": total, "total_interest_minor": total - t.AmountMinor}
	disclosures := map[string]any{"apr_bps": t.RateAPRBps, "total_cost_of_credit_minor": total - t.AmountMinor,
		"conditions": d.Conditions, "note": "Baseline disclosures; country packs define the regulated wording."}
	sJSON, _ := json.Marshal(schedule)
	dJSON, _ := json.Marshal(disclosures)
	expires := g.Now().UTC().Add(time.Duration(days) * 24 * time.Hour).Truncate(time.Second)
	id := newUUID()
	if _, err := tx.Exec(ctx, `INSERT INTO offers (id, tenant_id, application_id, decision_record_id, amount_minor, currency,
		term_months, rate_apr_bps, schedule_summary, disclosures, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		id, c.tenantID, c.app.ID, d.ID, t.AmountMinor, c.app.Currency, t.TermMonths, t.RateAPRBps, sJSON, dJSON, expires); err != nil {
		return nil, err
	}
	c.addEvent("OfferGenerated", map[string]any{"offer_id": id, "decision_record_id": d.ID, "amount_minor": t.AmountMinor,
		"term_months": t.TermMonths, "rate_apr_bps": t.RateAPRBps})
	return map[string]any{"offer_id": id, "expires_at": expires.Format(time.RFC3339), "schedule": schedule}, nil
}

// offer.send
func (g *Gateway) offerSend(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	o := c.app.offer(str(c.input, "offer_id"))
	if o == nil {
		return nil, newErr(ErrNotFound, "offer not found")
	}
	if o.Status != "draft" {
		return nil, newErr(ErrStateConflict, "offer is %s, not draft", o.Status)
	}
	channel := str(c.input, "channel")
	if channel == "" {
		channel = "portal"
	}
	to := c.app.Borrower
	if to == "" {
		to = "applicant"
	}
	msgRef, err := g.Connectors.Notify.Send(ctx, channel, to, "offer_sent", map[string]any{"offer_id": o.ID})
	if err != nil {
		return nil, newErr(ErrProviderUnavailable, "notification provider is unavailable")
	}
	if _, err := tx.Exec(ctx, `UPDATE offers SET status = 'sent', sent_at = now() WHERE id = $1`, o.ID); err != nil {
		return nil, err
	}
	c.addEvent("OfferSent", map[string]any{"offer_id": o.ID, "channel": channel, "message_ref": msgRef})
	return map[string]any{"offer_id": o.ID, "message_ref": msgRef}, nil
}

// offer.accept: only the borrower (or staff with the borrower's signed form) accepts.
func (g *Gateway) offerAccept(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	o := c.app.offer(str(c.input, "offer_id"))
	if o == nil {
		return nil, newErr(ErrNotFound, "offer not found")
	}
	if !g.Now().Before(o.ExpiresAt) {
		return nil, newErr("OFFER_EXPIRED", "the offer expired at %s", o.ExpiresAt.UTC().Format(time.RFC3339))
	}
	evidence, _ := json.Marshal(map[string]any{"signature": c.input["signature"], "accepted_by": c.p.ID,
		"accepted_at": g.Now().UTC().Format(time.RFC3339)})
	if _, err := tx.Exec(ctx, `UPDATE offers SET status = 'accepted', accepted_at = now(), acceptance_evidence = $2 WHERE id = $1`,
		o.ID, evidence); err != nil {
		return nil, err
	}
	c.addEvent("OfferAccepted", map[string]any{"offer_id": o.ID})
	return map[string]any{"offer_id": o.ID}, nil
}

// loan.handoff sends the booking package to the core banking system.
func (g *Gateway) loanHandoff(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	o := c.app.acceptedOffer()
	if o == nil {
		return nil, newErr(ErrPrecondition, "no accepted offer")
	}
	pkg := connectors.BookingPackage{ApplicationID: c.app.ID, Reference: c.app.Reference, Currency: o.Currency,
		AmountMinor: o.AmountMinor, TermMonths: o.TermMonths, RateAPRBps: o.RateAPRBps,
		MonthlyPaymentMin: int64(asFloat(o.Schedule["monthly_payment_minor"])), DecisionRecordID: o.DecisionID, OfferID: o.ID}
	for _, p := range c.app.Parties {
		pkg.Parties = append(pkg.Parties, p.connectorParty())
	}
	target := str(c.input, "target")
	res, err := g.Connectors.Core.Book(ctx, target, pkg)
	if err != nil {
		g.Log.Warn("hand-off failed", "target", target, "err", err)
		return nil, newErr(ErrTargetUnavailable, "hand-off target %q is unavailable", target)
	}
	h := map[string]any{"target": res.Target, "external_client_id": res.ExternalClientID,
		"external_loan_id": res.ExternalLoanID, "offer_id": o.ID, "booked_at": g.Now().UTC().Format(time.RFC3339)}
	hJSON, _ := json.Marshal(h)
	if _, err := tx.Exec(ctx, `UPDATE applications SET data = data || jsonb_build_object('handoff', $2::jsonb), closed_at = now()
		WHERE id = $1`, c.app.ID, hJSON); err != nil {
		return nil, err
	}
	c.addEvent("LoanHandedOff", h)
	return map[string]any{"external_client_id": res.ExternalClientID, "external_loan_id": res.ExternalLoanID}, nil
}
