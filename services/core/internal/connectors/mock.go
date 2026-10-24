package connectors

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Mock implements every port with deterministic, offline behaviour.
//
// Results are "healthy" by default. Markers in a party's legal name trigger other
// outcomes, so demos and tests can exercise every path:
//
//	SANCTION  sanctions screening returns a confirmed match (needs_review)
//	FAILKYC   identity verification fails
//	DEFAULT   the credit bureau reports a serious default in the last 24 months
//	THIN      bank analysis shows thin cash flow and frequent overdrafts
//	STARTUP   the business was incorporated 7 months ago
type Mock struct {
	Now func() time.Time
}

func has(p Party, marker string) bool {
	return strings.Contains(strings.ToUpper(p.LegalName), marker)
}

func ref(prefix string, parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return prefix + "-" + strings.ToUpper(hex.EncodeToString(h[:])[:10])
}

// VerifyIndividual implements IdentityVerifier.
func (m *Mock) VerifyIndividual(_ context.Context, p Party, level string) (CheckResult, error) {
	r := CheckResult{Status: "passed", Provider: "mock-kyc", ExternalRef: ref("KYC", p.ID, level),
		Result: map[string]any{"level": level, "document_verified": true, "liveness": "passed", "name_match": true}}
	if p.DateOfBirth == nil {
		r.Status = "needs_review"
		r.Result["issue"] = "date of birth missing"
	}
	if has(p, "FAILKYC") {
		r.Status = "failed"
		r.Result["document_verified"] = false
		r.Result["issue"] = "identity document could not be verified"
	}
	return r, nil
}

// VerifyBusiness implements IdentityVerifier.
func (m *Mock) VerifyBusiness(_ context.Context, p Party) (CheckResult, error) {
	incorporated := m.Now().AddDate(-4, 0, 0)
	if has(p, "STARTUP") {
		incorporated = m.Now().AddDate(0, -7, 0)
	}
	r := CheckResult{Status: "passed", Provider: "mock-kyb", ExternalRef: ref("KYB", p.ID),
		Result: map[string]any{
			"registration_number": p.RegistrationNumber,
			"registration_status": "active",
			"incorporated_on":     incorporated.Format("2006-01-02"),
			"country":             p.Country,
		}}
	if p.RegistrationNumber == "" {
		r.Status = "needs_review"
		r.Result["issue"] = "registration number missing"
	}
	if has(p, "FAILKYC") {
		r.Status = "failed"
		r.Result["registration_status"] = "not_found"
	}
	return r, nil
}

// Screen implements SanctionsScreener.
func (m *Mock) Screen(_ context.Context, p Party) (CheckResult, error) {
	r := CheckResult{Status: "passed", Provider: "mock-sanctions", ExternalRef: ref("SCR", p.ID),
		Result: map[string]any{"lists": []string{"UN", "OFAC", "EU"}, "potential_matches": 0, "confirmed_match": false}}
	if has(p, "SANCTION") {
		r.Status = "needs_review"
		r.Result["potential_matches"] = 1
		r.Result["confirmed_match"] = true
	}
	return r, nil
}

// Report implements CreditBureau.
func (m *Mock) Report(_ context.Context, p Party, reportType string) (CheckResult, error) {
	r := CheckResult{Status: "passed", Provider: "mock-bureau", ExternalRef: ref("BUR", p.ID, reportType),
		Result: map[string]any{"report_type": reportType, "score": 720, "serious_default_24m": false,
			"open_credit_lines": 2, "enquiries_6m": 1}}
	if has(p, "DEFAULT") {
		r.Result["score"] = 480
		r.Result["serious_default_24m"] = true
	}
	return r, nil
}

// Analyse implements BankData.
func (m *Mock) Analyse(_ context.Context, p Party, source string, requested int64) (CheckResult, error) {
	// Healthy business: revenue ~10x the requested amount, comfortable surplus.
	monthlyInflow := requested * 10 / 12
	netCashFlow := monthlyInflow / 5
	existingDebt := monthlyInflow / 50
	negativeDays := 1
	if has(p, "THIN") {
		monthlyInflow = requested * 3 / 12
		netCashFlow = monthlyInflow / 20
		existingDebt = monthlyInflow / 20
		negativeDays = 17
	}
	return CheckResult{Status: "passed", Provider: "mock-bank-data", ExternalRef: ref("BNK", p.ID, source),
		Result: map[string]any{
			"source":                          source,
			"months_covered":                  12,
			"avg_monthly_inflow_minor":        monthlyInflow,
			"avg_monthly_net_cash_flow_minor": netCashFlow,
			"existing_debt_service_minor":     existingDebt,
			"negative_balance_days_6m":        negativeDays,
		}}, nil
}

// Book implements CoreBanking.
func (m *Mock) Book(_ context.Context, target string, pkg BookingPackage) (BookingResult, error) {
	if target == "" {
		return BookingResult{}, fmt.Errorf("no hand-off target")
	}
	return BookingResult{Target: target, ExternalClientID: ref("CLI", pkg.ApplicationID), ExternalLoanID: ref("LN", pkg.ApplicationID, pkg.OfferID)}, nil
}

// Send implements Notifier.
func (m *Mock) Send(_ context.Context, channel, to, template string, _ map[string]any) (string, error) {
	return ref("MSG", channel, to, template, m.Now().String()), nil
}
