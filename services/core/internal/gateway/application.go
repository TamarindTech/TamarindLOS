package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
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
	Parties              []partyRecord
	RecommendedBy        string
	RecommendedFor       string
	Borrower             string
	Checks               map[string]any
}

type partyRecord struct {
	PartyID      string   `json:"party_id"`
	Role         string   `json:"role"`
	Kind         string   `json:"kind"`
	LegalName    string   `json:"legal_name"`
	Country      string   `json:"country,omitempty"`
	OwnershipPct *float64 `json:"ownership_pct,omitempty"`
}

// loadApplication reads an application and its related data. forUpdate locks the row.
func loadApplication(ctx context.Context, tx pgx.Tx, id string, forUpdate bool) (*appRecord, error) {
	if !isUUID(id) {
		return nil, newErr(ErrNotFound, "application %s not found", id)
	}
	q := `SELECT a.id::text, a.reference, p.code, a.state::text, a.state_version, a.state_machine_version,
		a.requested_amount_minor, a.currency, a.requested_term_months, COALESCE(a.purpose,''), a.channel,
		a.completeness_pct::float8, COALESCE(a.decision_outcome,''), a.created_by, a.created_at, a.submitted_at,
		COALESCE(a.data->>'borrower','')
		FROM applications a JOIN products p ON p.id = a.product_id WHERE a.id = $1`
	if forUpdate {
		q += ` FOR UPDATE OF a`
	}
	a := &appRecord{}
	err := tx.QueryRow(ctx, q, id).Scan(&a.ID, &a.Reference, &a.ProductCode, &a.State, &a.StateVersion, &a.StateMachineVersion,
		&a.RequestedAmountMinor, &a.Currency, &a.RequestedTermMonths, &a.Purpose, &a.Channel,
		&a.CompletenessPct, &a.DecisionOutcome, &a.CreatedBy, &a.CreatedAt, &a.SubmittedAt, &a.Borrower)
	if errors.Is(err, pgx.ErrNoRows) {
		// Row-level security also hides other tenants' applications, so they look "not found".
		return nil, newErr(ErrNotFound, "application %s not found", id)
	}
	if err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `SELECT p.id::text, ap.role::text, p.kind::text, p.legal_name, COALESCE(p.country,''), ap.ownership_pct::float8
		FROM application_parties ap JOIN parties p ON p.id = ap.party_id
		WHERE ap.application_id = $1 ORDER BY ap.added_at`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var pr partyRecord
		if err := rows.Scan(&pr.PartyID, &pr.Role, &pr.Kind, &pr.LegalName, &pr.Country, &pr.OwnershipPct); err != nil {
			rows.Close()
			return nil, err
		}
		a.Parties = append(a.Parties, pr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	err = tx.QueryRow(ctx, `SELECT actor, COALESCE(on_behalf_of,'') FROM decision_records
		WHERE application_id = $1 AND kind = 'recommendation' ORDER BY created_at DESC LIMIT 1`, id).Scan(&a.RecommendedBy, &a.RecommendedFor)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	var total, done, review int
	if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE status = 'passed'),
		count(*) FILTER (WHERE status IN ('failed','needs_review'))
		FROM checks WHERE application_id = $1`, id).Scan(&total, &done, &review); err != nil {
		return nil, err
	}
	// Phase 0: mandatory-check tracking arrives with the verification connectors.
	a.Checks = map[string]any{"all_mandatory_complete": false, "any_needs_review": review > 0, "count": total}
	return a, nil
}

// celMap is the `application` variable for CEL expressions.
func (a *appRecord) celMap() map[string]any {
	parties := make([]any, 0, len(a.Parties))
	for _, p := range a.Parties {
		parties = append(parties, map[string]any{"party_id": p.PartyID, "role": p.Role, "kind": p.Kind})
	}
	return map[string]any{
		"id":                       a.ID,
		"state":                    a.State,
		"state_version":            int64(a.StateVersion),
		"requested_amount_minor":   a.RequestedAmountMinor,
		"requested_term_months":    a.RequestedTermMonths,
		"currency":                 a.Currency,
		"completeness_pct":         a.CompletenessPct,
		"decision_outcome":         a.DecisionOutcome,
		"parties":                  parties,
		"has_recommendation":       a.RecommendedBy != "",
		"conditions_precedent_met": false,
	}
}

// view is the JSON representation returned by application.get.
func (a *appRecord) view() map[string]any {
	parties := a.Parties
	if parties == nil {
		parties = []partyRecord{}
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
		"created_by":             a.CreatedBy,
		"created_at":             a.CreatedAt.UTC().Format(time.RFC3339),
		"parties":                parties,
	}
	if a.SubmittedAt != nil {
		v["submitted_at"] = a.SubmittedAt.UTC().Format(time.RFC3339)
	}
	if a.DecisionOutcome != "" {
		v["decision_outcome"] = a.DecisionOutcome
	}
	return v
}

// applicationIDFor finds the application a call refers to.
func (g *Gateway) applicationIDFor(ctx context.Context, tx pgx.Tx, c *call) (string, error) {
	if id, ok := c.input["application_id"].(string); ok {
		return id, nil
	}
	if c.tool.Name == "approval.respond" {
		reqID, _ := c.input["approval_request_id"].(string)
		var appID *string
		err := tx.QueryRow(ctx, `SELECT application_id::text FROM approval_requests WHERE id = $1`, reqID).Scan(&appID)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", newErr(ErrNotFound, "approval request %s not found", reqID)
		}
		if err != nil {
			return "", err
		}
		if appID != nil {
			return *appID, nil
		}
	}
	return "", nil
}
