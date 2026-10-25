package policy_test

import (
	"testing"

	"github.com/TamarindTech/TamarindLOS/services/core/internal/catalogue"
	"github.com/TamarindTech/TamarindLOS/services/core/internal/policy"
)

const (
	toolsDir = "../../../../tools"
	packDir  = "../../../../policy-packs/baseline/sme-working-capital"
)

func load(t *testing.T) (*catalogue.Catalogue, *policy.Pack) {
	t.Helper()
	cat, err := catalogue.Load(toolsDir)
	if err != nil {
		t.Fatalf("catalogue: %v", err)
	}
	pack, err := policy.LoadPack(packDir)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	return cat, pack
}

func TestPackMatchesCatalogue(t *testing.T) {
	cat, pack := load(t)
	if n := len(cat.Names); n != 36 {
		t.Errorf("expected 36 tools, got %d", n)
	}
	for _, p := range pack.Validate(cat) {
		t.Error(p)
	}
}

func TestRuleCases(t *testing.T) {
	_, pack := load(t)
	results, err := pack.RunRuleTests()
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("no rule test cases found")
	}
	for _, r := range results {
		if !r.Passed {
			t.Errorf("%s: got %s %v, want %s %v", r.Name, r.Got, r.GotFailed, r.Want, r.WantFailed)
		}
	}
}

func TestMissingDataFailsRules(t *testing.T) {
	_, pack := load(t)
	ev := pack.EvaluateRules(map[string]any{})
	if ev.Outcome != "decline" {
		t.Fatalf("an empty application must not pass; got %s", ev.Outcome)
	}
}

func TestCedarScenarios(t *testing.T) {
	_, pack := load(t)
	const tenant, other = "tenant-a", "tenant-b"
	officer := policy.Subject{ID: "user:ana", TenantID: tenant, Roles: []string{"loan_officer"}}
	approver := policy.Subject{ID: "user:ben", TenantID: tenant, Roles: []string{"credit_approver"}, ApprovalLimitMinor: pack.ApprovalLimit([]string{"credit_approver"})}
	senior := policy.Subject{ID: "user:sam", TenantID: tenant, Roles: []string{"senior_credit_approver"}, ApprovalLimitMinor: pack.ApprovalLimit([]string{"senior_credit_approver"})}
	foreign := policy.Subject{ID: "user:zoe", TenantID: other, Roles: []string{"credit_approver"}, ApprovalLimitMinor: 99_999_999}
	uw := policy.Subject{ID: "agent:underwriting", TenantID: tenant, Roles: []string{"agent_underwriting"}}
	ver := policy.Subject{ID: "agent:verification", TenantID: tenant, Roles: []string{"agent_verification"}}
	intake := policy.Subject{ID: "agent:intake", TenantID: tenant, Roles: []string{"agent_intake"}}
	borrower := policy.Subject{ID: "borrower:b1", TenantID: tenant}
	stranger := policy.Subject{ID: "borrower:b2", TenantID: tenant}

	app := func(amount int64, recFor string) policy.Resource {
		return policy.Resource{ID: "app", TenantID: tenant, RequestedAmountMinor: amount,
			RecommendedBy: "agent:underwriting", RecommendedFor: recFor, Borrower: "borrower:b1"}
	}
	small, big := app(2_000_000, "user:ana"), app(5_000_000, "user:ana")
	selfRecommended := app(2_000_000, "user:ben")

	cases := []struct {
		name string
		s    policy.Subject
		tool string
		r    policy.Resource
		want bool
	}{
		{"approver approves within limit", approver, "decision.approve", small, true},
		{"approver over limit is denied", approver, "decision.approve", big, false},
		{"senior approver inherits approver role with higher limit", senior, "decision.approve", big, true},
		{"maker-checker: represented user cannot approve", approver, "decision.approve", selfRecommended, false},
		{"other tenant is denied", foreign, "decision.approve", small, false},
		{"agents can never approve", uw, "decision.approve", small, false},
		{"agents can never respond to approvals", intake, "approval.respond", small, false},
		{"underwriting agent can recommend", uw, "decision.recommend", small, true},
		{"underwriting agent cannot run KYC", uw, "kyc.run", small, false},
		{"verification agent can run KYC", ver, "kyc.run", small, true},
		{"intake agent can create applications", intake, "application.create", policy.Resource{ID: "new", TenantID: tenant}, true},
		{"intake agent can withdraw (gated by autonomy, not policy)", intake, "application.withdraw", small, true},
		{"officer can recommend", officer, "decision.recommend", small, true},
		{"officer cannot approve", officer, "decision.approve", small, false},
		{"officer can respond to approval requests", officer, "approval.respond", small, true},
		{"borrower accepts own offer", borrower, "offer.accept", small, true},
		{"borrower cannot act on another application", stranger, "offer.accept", small, false},
		{"borrower cannot create applications for others", borrower, "decision.recommend", small, false},
	}
	for _, c := range cases {
		d := pack.Authz.Authorize(c.s, c.tool, c.r)
		if len(d.Errors) > 0 {
			t.Errorf("%s: evaluation errors %v", c.name, d.Errors)
		}
		if d.Allowed != c.want {
			t.Errorf("%s: allowed=%v, want %v (policies %v)", c.name, d.Allowed, c.want, d.Reasons)
		}
	}
}

func TestEffectiveAutonomy(t *testing.T) {
	cat, pack := load(t)
	cases := []struct {
		tool, trust, want string
		outcome           policy.AgentOutcome
	}{
		{"application.create", "L3", "L3", policy.AgentExecute},
		{"application.withdraw", "L3", "L2", policy.AgentApproval},
		{"decision.recommend", "L3", "L2", policy.AgentApproval},
		{"decision.approve", "L4", "gate", policy.AgentApproval},
		{"offer.accept", "L4", "L0", policy.AgentDeny},
		{"application.create", "L1", "L1", policy.AgentDeny},
		{"application.get", "L3", "L3", policy.AgentExecute},
	}
	for _, c := range cases {
		got := policy.EffectiveAutonomy(cat.Tools[c.tool], pack.Autonomy[c.tool], "", c.trust)
		if got != c.want || policy.OutcomeFor(got) != c.outcome {
			t.Errorf("%s with trust %s: got %s (%s), want %s (%s)", c.tool, c.trust, got, policy.OutcomeFor(got), c.want, c.outcome)
		}
	}
}
