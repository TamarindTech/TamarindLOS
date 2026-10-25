// Package connectors defines the ports Tamarind uses to reach external services
// (Reference Architecture §12) and ships mock adapters for every one of them, so the
// whole system runs end to end without external accounts.
package connectors

import (
	"context"
	"time"
)

// Party is the data a connector receives about a party.
type Party struct {
	ID                 string
	Kind               string // individual | business
	Role               string
	LegalName          string
	RegistrationNumber string
	Country            string
	DateOfBirth        *time.Time
}

// CheckResult is the normalised outcome of an external check.
type CheckResult struct {
	Status      string         // passed | failed | needs_review
	Provider    string         // adapter name, e.g. "mock-kyc"
	ExternalRef string         // provider's reference
	Result      map[string]any // provider data, normalised
}

// IdentityVerifier performs KYC (individuals) and KYB (businesses).
type IdentityVerifier interface {
	VerifyIndividual(ctx context.Context, p Party, level string) (CheckResult, error)
	VerifyBusiness(ctx context.Context, p Party) (CheckResult, error)
}

// SanctionsScreener screens a party against sanctions and PEP lists.
type SanctionsScreener interface {
	Screen(ctx context.Context, p Party) (CheckResult, error)
}

// CreditBureau returns consumer or commercial credit reports.
type CreditBureau interface {
	Report(ctx context.Context, p Party, reportType string) (CheckResult, error)
}

// BankData analyses bank transactions into cash-flow metrics.
type BankData interface {
	Analyse(ctx context.Context, p Party, source string, requestedAmountMinor int64) (CheckResult, error)
}

// BookingPackage is what Tamarind hands to a core banking or servicing system.
type BookingPackage struct {
	ApplicationID     string
	Reference         string
	Currency          string
	AmountMinor       int64
	TermMonths        int
	RateAPRBps        int
	MonthlyPaymentMin int64
	Parties           []Party
	DecisionRecordID  string
	OfferID           string
}

// BookingResult identifies the loan in the target system.
type BookingResult struct {
	Target           string
	ExternalClientID string
	ExternalLoanID   string
}

// CoreBanking books approved, accepted loans.
type CoreBanking interface {
	Book(ctx context.Context, target string, pkg BookingPackage) (BookingResult, error)
}

// Notifier sends messages to parties.
type Notifier interface {
	Send(ctx context.Context, channel, to, template string, data map[string]any) (string, error)
}

// Set bundles the adapters the gateway uses.
type Set struct {
	Identity  IdentityVerifier
	Sanctions SanctionsScreener
	Bureau    CreditBureau
	Bank      BankData
	Core      CoreBanking
	Notify    Notifier
	Blobs     BlobStore
	Malware   MalwareScanner
	Classify  DocumentClassifier
	Extract   Extractor
}

// Mocks returns a Set made of mock adapters, storing uploads under blobDir.
func Mocks(now func() time.Time, blobDir string) Set {
	m := &Mock{Now: now}
	return Set{Identity: m, Sanctions: m, Bureau: m, Bank: m, Core: m, Notify: m,
		Blobs: FSBlobStore{Dir: blobDir}, Malware: m, Classify: m, Extract: m}
}
