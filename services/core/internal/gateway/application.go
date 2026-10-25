package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamarindTech/TamarindLOS/services/core/internal/policy"
)

// appRecord is the application snapshot used by policies, preconditions and handlers.
type appRecord struct {
	ID                   string
	Reference            string
	ProductCode          string
	State                string
	StateVersion         int
	StateMachineVersion  string
	RequestedAmountMinor int64
	Currency             string
	RequestedTermMonths  int64
	Purpose              string
	Channel              string
	CompletenessPct      float64
	DecisionOutcome      string
	CreatedBy            string
	CreatedAt            time.Time
	SubmittedAt          *time.Time
	Borrower             string
	Data                 map[string]any

	Parties        []partyRecord
	Consents       []consentRecord
	Checks         []checkRecord
	Recommendation *decisionRecord // latest recommendation
	Decision       *decisionRecord // latest final decision
	Offers         []offerRecord
	Documents      []documentRecord
	Evidence       []evidenceRecord

	// Derived by summarize().
	RecommendedBy    string
	RecommendedFor   string
	ChecksSummary    map[string]any
	Metrics          map[string]any
	MissingConsents  []string
	MissingDocuments []string
	// MissingDocumentItems is MissingDocuments in a form agents can act on.
	MissingDocumentItems []map[string]any
	Business             map[string]any
	Directors            []any
}

type partyRecord struct {
	PartyID            string     `json:"party_id"`
	Role               string     `json:"role"`
	Kind               string     `json:"kind"`
	LegalName          string     `json:"legal_name"`
	Country            string     `json:"country,omitempty"`
	RegistrationNumber string     `json:"registration_number,omitempty"`
	DateOfBirth        *time.Time `json:"-"`
	DOB                string     `json:"date_of_birth,omitempty"`
	OwnershipPct       *float64   `json:"ownership_pct,omitempty"`
}

type documentRecord struct {
	ID           string    `json:"document_id"`
	PartyID      string    `json:"party_id,omitempty"`
	DocumentType string    `json:"document_type,omitempty"`
	DeclaredType string    `json:"declared_type,omitempty"`
	Confidence   *float64  `json:"classification_confidence,omitempty"`
	Status       string    `json:"status"`
	FileName     string    `json:"file_name,omitempty"`
	MimeType     string    `json:"mime_type"`
	SizeBytes    int64     `json:"size_bytes"`
	SHA256       string    `json:"sha256"`
	StorageKey   string    `json:"-"`
	UploadedBy   string    `json:"uploaded_by"`
	CreatedAt    time.Time `json:"created_at"`
}

type evidenceRecord struct {
	ID         string  `json:"evidence_id"`
	DocumentID string  `json:"document_id"`
	Field      string  `json:"field"`
	Value      any     `json:"value"`
	Confidence float64 `json:"confidence"`
	Status     string  `json:"status"`
	VerifiedBy string  `json:"verified_by,omitempty"`
	Note       string  `json:"note,omitempty"`
}

type consentRecord struct {
	ID      string `json:"consent_id"`
	PartyID string `json:"party_id"`
	Scope   string `json:"scope"`
}

type checkRecord struct {
	ID          string         `json:"check_id"`
	PartyID     string         `json:"party_id"`
	Type        string         `json:"check_type"`
	Status      string         `json:"status"`
	Provider    string         `json:"provider"`
	Result      map[string]any `json:"result"`
	RequestedAt time.Time      `json:"requested_at"`
}

type decisionRecord struct {
	ID          string         `json:"decision_record_id"`
	Kind        string         `json:"kind"`
	Outcome     string         `json:"outcome"`
	Terms       map[string]any `json:"terms,omitempty"`
	Conditions  []any          `json:"conditions,omitempty"`
	ReasonCodes []string       `json:"reason_codes,omitempty"`
	Actor       string         `json:"actor"`
	OnBehalfOf  string         `json:"on_behalf_of,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
}

type offerRecord struct {
	ID          string    `json:"offer_id"`
	Status      string    `json:"status"`
	AmountMinor int64     `json:"amount_minor"`
	Currency    string    `json:"currency"`
	TermMonths  int       `json:"term_months"`
	RateAPRBps  int       `json:"rate_apr_bps"`
	ExpiresAt   time.Time `json:"expires_at"`
	DecisionID  string    `json:"decision_record_id"`
	Schedule    map[string]any
}

func (a *appRecord) party(id string) *partyRecord {
	for i := range a.Parties {
		if a.Parties[i].PartyID == id {
			return &a.Parties[i]
		}
	}
	return nil
}

func (a *appRecord) offer(id string) *offerRecord {
	for i := range a.Offers {
		if a.Offers[i].ID == id {
			return &a.Offers[i]
		}
	}
	return nil
}

func (a *appRecord) acceptedOffer() *offerRecord {
	for i := range a.Offers {
		if a.Offers[i].Status == "accepted" {
			return &a.Offers[i]
		}
	}
	return nil
}

func jsonMap(b []byte) map[string]any {
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	return m
}

// loadApplication reads an application and its related data. forUpdate locks the row.
func (g *Gateway) loadApplication(ctx context.Context, tx pgx.Tx, id string, forUpdate bool) (*appRecord, error) {
	if !isUUID(id) {
		return nil, newErr(ErrNotFound, "application %s not found", id)
	}
	q := `SELECT a.id::text, a.reference, p.code, a.state::text, a.state_version, a.state_machine_version,
		a.requested_amount_minor, a.currency, a.requested_term_months, COALESCE(a.purpose,''), a.channel,
		a.completeness_pct::float8, COALESCE(a.decision_outcome,''), a.created_by, a.created_at, a.submitted_at,
		COALESCE(a.data->>'borrower',''), a.data
		FROM applications a JOIN products p ON p.id = a.product_id WHERE a.id = $1`
	if forUpdate {
		q += ` FOR UPDATE OF a`
	}
	a := &appRecord{}
	var data []byte
	err := tx.QueryRow(ctx, q, id).Scan(&a.ID, &a.Reference, &a.ProductCode, &a.State, &a.StateVersion, &a.StateMachineVersion,
		&a.RequestedAmountMinor, &a.Currency, &a.RequestedTermMonths, &a.Purpose, &a.Channel,
		&a.CompletenessPct, &a.DecisionOutcome, &a.CreatedBy, &a.CreatedAt, &a.SubmittedAt, &a.Borrower, &data)
	if errors.Is(err, pgx.ErrNoRows) {
		// Row-level security also hides other tenants' applications, so they look "not found".
		return nil, newErr(ErrNotFound, "application %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	a.Data = jsonMap(data)

	// Parties.
	rows, err := tx.Query(ctx, `SELECT p.id::text, ap.role::text, p.kind::text, p.legal_name, COALESCE(p.country,''),
		COALESCE(p.registration_number,''), p.date_of_birth, ap.ownership_pct::float8
		FROM application_parties ap JOIN parties p ON p.id = ap.party_id
		WHERE ap.application_id = $1 ORDER BY ap.added_at`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var pr partyRecord
		if err := rows.Scan(&pr.PartyID, &pr.Role, &pr.Kind, &pr.LegalName, &pr.Country, &pr.RegistrationNumber,
			&pr.DateOfBirth, &pr.OwnershipPct); err != nil {
			rows.Close()
			return nil, err
		}
		if pr.DateOfBirth != nil {
			pr.DOB = pr.DateOfBirth.Format("2006-01-02")
		}
		a.Parties = append(a.Parties, pr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Valid consents for those parties.
	rows, err = tx.Query(ctx, `SELECT c.id::text, c.party_id::text, c.scope FROM consents c
		JOIN application_parties ap ON ap.party_id = c.party_id AND ap.application_id = $1
		WHERE c.revoked_at IS NULL AND (c.expires_at IS NULL OR c.expires_at > now())`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var cr consentRecord
		if err := rows.Scan(&cr.ID, &cr.PartyID, &cr.Scope); err != nil {
			rows.Close()
			return nil, err
		}
		a.Consents = append(a.Consents, cr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Checks, oldest first (so later checks override earlier ones).
	rows, err = tx.Query(ctx, `SELECT id::text, COALESCE(party_id::text,''), check_type, status::text, provider, result, requested_at
		FROM checks WHERE application_id = $1 ORDER BY requested_at, id`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var cr checkRecord
		var res []byte
		if err := rows.Scan(&cr.ID, &cr.PartyID, &cr.Type, &cr.Status, &cr.Provider, &res, &cr.RequestedAt); err != nil {
			rows.Close()
			return nil, err
		}
		cr.Result = jsonMap(res)
		a.Checks = append(a.Checks, cr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Latest recommendation and latest final decision.
	for _, kind := range []string{"recommendation", "decision"} {
		var d decisionRecord
		var terms, conds []byte
		var onBehalf *string
		err := tx.QueryRow(ctx, `SELECT id::text, kind, outcome, terms, conditions, reason_codes, actor, on_behalf_of, created_at
			FROM decision_records WHERE application_id = $1 AND kind = $2 ORDER BY created_at DESC LIMIT 1`, id, kind).
			Scan(&d.ID, &d.Kind, &d.Outcome, &terms, &conds, &d.ReasonCodes, &d.Actor, &onBehalf, &d.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if onBehalf != nil {
			d.OnBehalfOf = *onBehalf
		}
		if len(terms) > 0 {
			d.Terms = jsonMap(terms)
		}
		_ = json.Unmarshal(conds, &d.Conditions)
		if kind == "recommendation" {
			a.Recommendation = &d
		} else {
			a.Decision = &d
		}
	}

	// Offers.
	rows, err = tx.Query(ctx, `SELECT id::text, status::text, amount_minor, currency, term_months, rate_apr_bps, expires_at,
		decision_record_id::text, schedule_summary FROM offers WHERE application_id = $1 ORDER BY created_at`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var o offerRecord
		var sched []byte
		if err := rows.Scan(&o.ID, &o.Status, &o.AmountMinor, &o.Currency, &o.TermMonths, &o.RateAPRBps, &o.ExpiresAt,
			&o.DecisionID, &sched); err != nil {
			rows.Close()
			return nil, err
		}
		o.Schedule = jsonMap(sched)
		a.Offers = append(a.Offers, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Documents and extracted evidence.
	rows, err = tx.Query(ctx, `SELECT id::text, COALESCE(party_id::text,''), COALESCE(document_type,''), COALESCE(declared_type,''),
		classification_confidence::float8, status, COALESCE(file_name,''), mime_type, size_bytes, sha256, storage_key, uploaded_by, created_at
		FROM documents WHERE application_id = $1 ORDER BY created_at, id`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d documentRecord
		if err := rows.Scan(&d.ID, &d.PartyID, &d.DocumentType, &d.DeclaredType, &d.Confidence, &d.Status, &d.FileName,
			&d.MimeType, &d.SizeBytes, &d.SHA256, &d.StorageKey, &d.UploadedBy, &d.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		a.Documents = append(a.Documents, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, `SELECT id::text, document_id::text, field, value, confidence::float8, status::text,
		COALESCE(verified_by,''), COALESCE(note,'') FROM evidence WHERE application_id = $1 ORDER BY created_at, id`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var e evidenceRecord
		var v []byte
		if err := rows.Scan(&e.ID, &e.DocumentID, &e.Field, &v, &e.Confidence, &e.Status, &e.VerifiedBy, &e.Note); err != nil {
			rows.Close()
			return nil, err
		}
		_ = json.Unmarshal(v, &e.Value)
		a.Evidence = append(a.Evidence, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	g.summarize(a)
	return a, nil
}

// latestCheck returns the most recent check of a type (and, for credit reports, report type) for a party.
func (a *appRecord) latestCheck(partyID, checkType, report string) *checkRecord {
	var found *checkRecord
	for i := range a.Checks {
		c := &a.Checks[i]
		if c.PartyID != partyID {
			continue
		}
		t := checkType
		if t == "kyb" || t == "kyc" {
			if c.Type != t {
				continue
			}
		} else if c.Type != t {
			continue
		}
		if report != "" && fmt.Sprint(c.Result["report_type"]) != report {
			continue
		}
		found = c
	}
	return found
}

// requiredDocuments lists the pack's document requirements that apply to this application.
func (g *Gateway) requiredDocuments(a *appRecord) []policy.DocumentRequirement {
	var out []policy.DocumentRequirement
	for _, d := range g.Pack.Documents {
		if d.Required {
			out = append(out, d)
			continue
		}
		if d.RequiredWhen == "" {
			continue
		}
		ok, err := g.Pack.Exprs.EvalBool(d.RequiredWhen, map[string]any{"application": map[string]any{
			"requested_amount_minor": a.RequestedAmountMinor, "requested_term_months": a.RequestedTermMonths}})
		if err == nil && ok {
			out = append(out, d)
		}
	}
	return out
}

// hasDocument reports whether a party has a classified document of a type. Documents
// attached without a party count for the applicant.
func (a *appRecord) hasDocument(p partyRecord, docType string) bool {
	for _, d := range a.Documents {
		owner := d.PartyID
		if owner == "" && p.Role == "applicant" {
			owner = p.PartyID
		}
		if owner == p.PartyID && d.DocumentType == docType && (d.Status == "classified" || d.Status == "extracted") {
			return true
		}
	}
	return false
}

func (a *appRecord) document(id string) *documentRecord {
	for i := range a.Documents {
		if a.Documents[i].ID == id {
			return &a.Documents[i]
		}
	}
	return nil
}

func (a *appRecord) evidence(id string) *evidenceRecord {
	for i := range a.Evidence {
		if a.Evidence[i].ID == id {
			return &a.Evidence[i]
		}
	}
	return nil
}

func (a *appRecord) hasConsent(partyID, scope string) bool {
	for _, c := range a.Consents {
		if c.PartyID == partyID && c.Scope == scope {
			return true
		}
	}
	return false
}

func asFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

// monthlyPayment returns the amortising monthly payment in minor units.
func monthlyPayment(principal int64, months int, aprBps int) int64 {
	if months <= 0 {
		return 0
	}
	r := float64(aprBps) / 10000 / 12
	if r == 0 {
		return int64(math.Round(float64(principal) / float64(months)))
	}
	p := float64(principal) * r / (1 - math.Pow(1+r, -float64(months)))
	return int64(math.Round(p))
}

// summarize derives completeness, check status, metrics and party facts for policies.
func (g *Gateway) summarize(a *appRecord) {
	if a.Recommendation != nil {
		a.RecommendedBy, a.RecommendedFor = a.Recommendation.Actor, a.Recommendation.OnBehalfOf
	}
	pack := g.Pack
	now := g.Now().UTC()

	// Completeness: required parties, the consents mandatory checks need, and the
	// required documents (classified, not rejected) for each party they apply to.
	var items, satisfied int
	for _, req := range pack.Parties.Required {
		n := 0
		for _, p := range a.Parties {
			if p.Role == req.Role {
				n++
			}
		}
		items += req.Min
		satisfied += min(n, req.Min)
	}
	a.MissingConsents = nil
	for _, req := range pack.Checks {
		if !req.Mandatory || req.Consent == "" {
			continue
		}
		for _, p := range a.Parties {
			if !req.AppliesTo(p.Role) {
				continue
			}
			items++
			if a.hasConsent(p.PartyID, req.Consent) {
				satisfied++
			} else {
				a.MissingConsents = append(a.MissingConsents, fmt.Sprintf("%s consent from %s (%s)", req.Consent, p.LegalName, p.Role))
			}
		}
	}
	a.MissingDocuments, a.MissingDocumentItems = nil, nil
	for _, req := range g.requiredDocuments(a) {
		for _, p := range a.Parties {
			if !req.AppliesTo(p.Role) {
				continue
			}
			items++
			if a.hasDocument(p, req.Type) {
				satisfied++
			} else {
				a.MissingDocuments = append(a.MissingDocuments, fmt.Sprintf("%s for %s (%s)", req.Type, p.LegalName, p.Role))
				a.MissingDocumentItems = append(a.MissingDocumentItems, map[string]any{"document_type": req.Type,
					"party_id": p.PartyID, "party_role": p.Role})
			}
		}
	}
	if items > 0 {
		a.CompletenessPct = math.Round(float64(satisfied)*10000/float64(items)) / 100
	}

	// Mandatory checks.
	total, done, review := 0, 0, false
	kycAll, kycSeen := true, false
	kybStatus := ""
	for _, req := range pack.Checks {
		for _, p := range a.Parties {
			if !req.AppliesTo(p.Role) {
				continue
			}
			c := a.latestCheck(p.PartyID, req.Type, req.Report)
			if req.Mandatory {
				total++
				if c != nil && c.Status != "pending" && c.Status != "error" {
					done++
				}
				if c != nil && (c.Status == "failed" || c.Status == "needs_review") {
					review = true
				}
			}
			switch req.Type {
			case "kyc":
				kycSeen = true
				if c == nil || c.Status != "passed" {
					kycAll = false
				}
			case "kyb":
				if c != nil {
					kybStatus = c.Status
				}
			}
		}
	}
	sanctionsMatch, seriousDefault := false, false
	for _, c := range a.Checks {
		if c.Type == "sanctions" && c.Result["confirmed_match"] == true {
			sanctionsMatch = true
		}
		if c.Type == "credit_bureau" && c.Result["serious_default_24m"] == true {
			seriousDefault = true
		}
	}
	a.ChecksSummary = map[string]any{
		"all_mandatory_complete": total > 0 && done == total,
		"any_needs_review":       review,
		"mandatory_total":        int64(total),
		"mandatory_done":         int64(done),
		"sanctions":              map[string]any{"confirmed_match": sanctionsMatch},
		"kyc":                    map[string]any{"all_passed": kycSeen && kycAll},
		"kyb":                    map[string]any{"status": kybStatus},
		"bureau":                 map[string]any{"serious_default_24m": seriousDefault},
	}

	// Business facts and directors.
	a.Business = map[string]any{}
	a.Directors = []any{}
	for _, p := range a.Parties {
		switch {
		case p.Role == "applicant":
			if c := a.latestCheck(p.PartyID, "kyb", ""); c != nil {
				if s, ok := c.Result["incorporated_on"].(string); ok {
					if t, err := time.Parse("2006-01-02", s); err == nil {
						months := (now.Year()-t.Year())*12 + int(now.Month()-t.Month())
						if now.Day() < t.Day() {
							months--
						}
						a.Business["months_trading"] = int64(months)
					}
				}
				if s, ok := c.Result["registration_status"].(string); ok {
					a.Business["registration_status"] = s
				}
			}
		case p.Role == "director" && p.DateOfBirth != nil:
			age := now.Year() - p.DateOfBirth.Year()
			if now.YearDay() < p.DateOfBirth.YearDay() {
				age--
			}
			a.Directors = append(a.Directors, map[string]any{"age_years": int64(age)})
		}
	}

	// Cash-flow metrics from the applicant's bank analysis.
	a.Metrics = map[string]any{}
	for _, p := range a.Parties {
		if p.Role != "applicant" {
			continue
		}
		c := a.latestCheck(p.PartyID, "bank_analysis", "")
		if c == nil {
			continue
		}
		inflow := asFloat(c.Result["avg_monthly_inflow_minor"])
		net := asFloat(c.Result["avg_monthly_net_cash_flow_minor"])
		existing := asFloat(c.Result["existing_debt_service_minor"])
		newPayment := float64(monthlyPayment(a.RequestedAmountMinor, int(a.RequestedTermMonths), pack.Product.ReferenceAPRBps()))
		dscr := 0.0
		if existing+newPayment > 0 {
			dscr = math.Round(net/(existing+newPayment)*100) / 100
		}
		a.Metrics = map[string]any{
			"bank_months":                 int64(asFloat(c.Result["months_covered"])),
			"negative_balance_days_6m":    int64(asFloat(c.Result["negative_balance_days_6m"])),
			"annual_revenue_minor":        int64(inflow * 12),
			"avg_monthly_net_cash_flow":   int64(net),
			"existing_debt_service_minor": int64(existing),
			"estimated_new_payment_minor": int64(newPayment),
			"dscr":                        dscr,
			"reference_apr_bps":           int64(pack.Product.ReferenceAPRBps()),
		}
	}
}

// celMap is the `application` variable for CEL expressions.
func (a *appRecord) celMap() map[string]any {
	parties := make([]any, 0, len(a.Parties))
	for _, p := range a.Parties {
		parties = append(parties, map[string]any{"party_id": p.PartyID, "role": p.Role, "kind": p.Kind})
	}
	decision := ""
	if a.Decision != nil {
		decision = a.Decision.Outcome
	}
	return map[string]any{
		"id":                       a.ID,
		"state":                    a.State,
		"state_version":            int64(a.StateVersion),
		"requested_amount_minor":   a.RequestedAmountMinor,
		"requested_term_months":    a.RequestedTermMonths,
		"currency":                 a.Currency,
		"completeness_pct":         a.CompletenessPct,
		"decision_outcome":         decision,
		"parties":                  parties,
		"business":                 a.Business,
		"directors":                a.Directors,
		"has_recommendation":       a.Recommendation != nil,
		"conditions_precedent_met": a.acceptedOffer() != nil,
	}
}

// view is the JSON representation returned by application.get.
func (a *appRecord) view() map[string]any {
	parties := a.Parties
	if parties == nil {
		parties = []partyRecord{}
	}
	checks := []map[string]any{}
	for _, c := range a.Checks {
		checks = append(checks, map[string]any{"check_id": c.ID, "party_id": c.PartyID, "check_type": c.Type,
			"status": c.Status, "provider": c.Provider})
	}
	consents := a.Consents
	if consents == nil {
		consents = []consentRecord{}
	}
	offers := []map[string]any{}
	for _, o := range a.Offers {
		offers = append(offers, map[string]any{"offer_id": o.ID, "status": o.Status, "amount_minor": o.AmountMinor,
			"term_months": o.TermMonths, "rate_apr_bps": o.RateAPRBps, "expires_at": o.ExpiresAt.UTC().Format(time.RFC3339),
			"monthly_payment_minor": o.Schedule["monthly_payment_minor"]})
	}
	v := map[string]any{
		"application_id":         a.ID,
		"reference":              a.Reference,
		"product_code":           a.ProductCode,
		"state":                  a.State,
		"state_version":          a.StateVersion,
		"state_machine_version":  a.StateMachineVersion,
		"requested_amount_minor": a.RequestedAmountMinor,
		"currency":               a.Currency,
		"requested_term_months":  a.RequestedTermMonths,
		"purpose":                a.Purpose,
		"channel":                a.Channel,
		"completeness_pct":       a.CompletenessPct,
		"missing_consents":       nonNil(a.MissingConsents),
		"missing_documents":      nonNil(a.MissingDocuments),
		"missing_document_items": nonNilSlice(a.MissingDocumentItems),
		"documents":              nonNilSlice(a.Documents),
		"evidence":               nonNilSlice(a.Evidence),
		"created_by":             a.CreatedBy,
		"created_at":             a.CreatedAt.UTC().Format(time.RFC3339),
		"parties":                parties,
		"consents":               consents,
		"checks":                 checks,
		"checks_summary":         a.ChecksSummary,
		"metrics":                a.Metrics,
		"offers":                 offers,
	}
	if a.Borrower != "" {
		v["borrower"] = a.Borrower
	}
	if a.SubmittedAt != nil {
		v["submitted_at"] = a.SubmittedAt.UTC().Format(time.RFC3339)
	}
	if a.Recommendation != nil {
		v["recommendation"] = a.Recommendation
	}
	if a.Decision != nil {
		v["decision"] = a.Decision
	}
	if h, ok := a.Data["handoff"]; ok {
		v["handoff"] = h
	}
	return v
}

func nonNilSlice[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// applicationIDFor finds the application a call refers to.
func (g *Gateway) applicationIDFor(ctx context.Context, tx pgx.Tx, c *call) (string, error) {
	if id, ok := c.input["application_id"].(string); ok {
		return id, nil
	}
	lookup := func(table, key string) (string, error) {
		id, _ := c.input[key].(string)
		if !isUUID(id) {
			return "", newErr(ErrNotFound, "%s %s not found", key, id)
		}
		var appID *string
		err := tx.QueryRow(ctx, `SELECT application_id::text FROM `+table+` WHERE id = $1`, id).Scan(&appID)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", newErr(ErrNotFound, "%s %s not found", key, id)
		}
		if err != nil {
			return "", err
		}
		if appID == nil {
			return "", nil
		}
		return *appID, nil
	}
	switch {
	case c.tool.Name == "approval.respond":
		return lookup("approval_requests", "approval_request_id")
	case c.input["offer_id"] != nil:
		return lookup("offers", "offer_id")
	case c.input["document_id"] != nil:
		return lookup("documents", "document_id")
	case c.input["evidence_id"] != nil:
		return lookup("evidence", "evidence_id")
	case c.input["task_id"] != nil:
		return lookup("tasks", "task_id")
	case c.input["run_id"] != nil:
		return lookup("agent_runs", "run_id")
	}
	return "", nil
}
