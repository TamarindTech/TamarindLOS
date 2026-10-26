package gateway

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/TamarindTech/TamarindLOS/services/core/internal/policy"
)

// Risk-based pricing. The pack's scorecard grades the application and the grade sets
// the APR. A person may price differently, with a reason that is kept in the decision
// record; agents may not, and nobody may go below the cheapest grade's price.

// riskGrade scores the application with the pack's scorecard (nil without one).
func (g *Gateway) riskGrade(c *call) *policy.RiskGrade {
	if c.app == nil {
		return nil
	}
	return g.Pack.GradeRisk(g.celVars(c))
}

// defaultRate is the rate for approving terms when the caller gives none.
func (g *Gateway) defaultRate(rg *policy.RiskGrade) int64 {
	if rg != nil && rg.APRBps > 0 {
		return int64(rg.APRBps)
	}
	return int64(g.Pack.Product.ReferenceAPRBps())
}

// checkPricing compares the terms' rate with the graded price and returns the pricing
// record for the decision. proposer is who proposed the terms (an agent, for an approved
// agent proposal).
func (g *Gateway) checkPricing(c *call, rg *policy.RiskGrade, t terms, proposer string) (map[string]any, *ToolError) {
	if rg == nil {
		return map[string]any{"method": "reference_rate", "apr_bps": t.RateAPRBps}, nil
	}
	pricing := map[string]any{"method": "risk_grade_table", "risk_grade": rg.Grade, "points": rg.Points,
		"max_points": rg.MaxPoints, "scorecard": rg.Scorecard, "graded_apr_bps": rg.APRBps, "apr_bps": t.RateAPRBps}
	if len(rg.Missing) > 0 {
		pricing["missing_factors"] = rg.Missing
	}
	if t.RateAPRBps == int64(rg.APRBps) {
		return pricing, nil
	}
	if strings.HasPrefix(proposer, "agent:") || c.p.IsAgent() {
		return nil, &ToolError{Code: ErrPolicyDenied, Message: "agents price at the risk grade; only a person may set another rate",
			Details: map[string]any{"risk_grade": rg.Grade, "graded_apr_bps": rg.APRBps}}
	}
	if floor := g.Pack.Product.LowestAPRBps(); t.RateAPRBps < int64(floor) {
		return nil, &ToolError{Code: ErrValidation, Message: "rate is below the cheapest price in the grade table",
			Details: map[string]any{"floor_apr_bps": floor}}
	}
	reason := str(c.input, "pricing_override_reason")
	if reason == "" {
		return nil, &ToolError{Code: ErrValidation, Message: "the rate differs from the risk-grade price; give pricing_override_reason",
			Details: map[string]any{"risk_grade": rg.Grade, "graded_apr_bps": rg.APRBps}}
	}
	pricing["override"] = map[string]any{"by": c.p.ID, "reason": reason, "delta_bps": t.RateAPRBps - int64(rg.APRBps)}
	return pricing, nil
}

// recommendPreflight checks approving terms and their price before decision.recommend
// is executed or parked for approval, so an agent proposing an off-grade rate is refused
// at once instead of when a person tries to approve it.
func (g *Gateway) recommendPreflight(_ context.Context, _ pgx.Tx, c *call) *ToolError {
	outcome := str(c.input, "outcome")
	if outcome != "approve" && outcome != "approve_with_conditions" {
		return nil
	}
	rg := g.riskGrade(c)
	t := termsFrom(mapOf(c.input["terms"]), terms{AmountMinor: c.app.RequestedAmountMinor,
		TermMonths: c.app.RequestedTermMonths, RateAPRBps: g.defaultRate(rg)})
	if e := g.validTerms(c.app, t); e != nil {
		return e
	}
	proposer := c.p.ID
	if c.proposedBy != "" {
		proposer = c.proposedBy
	}
	_, e := g.checkPricing(c, rg, t, proposer)
	return e
}

// withScorecard adds the scorecard to the models a decision lists, once.
func withScorecard(models any, rg *policy.RiskGrade) any {
	if rg == nil {
		return models
	}
	list, _ := models.([]any)
	for _, m := range list {
		if mm, ok := m.(map[string]any); ok && mm["kind"] == "scorecard" {
			return models
		}
	}
	id, version, _ := strings.Cut(rg.Scorecard, "@")
	return append(list, map[string]any{"kind": "scorecard", "id": id, "version": version})
}

// riskView is the grade as shown to staff and agents.
func riskView(rg *policy.RiskGrade, provisional bool) map[string]any {
	return map[string]any{"grade": rg.Grade, "points": rg.Points, "max_points": rg.MaxPoints, "apr_bps": rg.APRBps,
		"factors": rg.Factors, "missing": rg.Missing, "scorecard": rg.Scorecard, "provisional": provisional}
}
