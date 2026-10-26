package policy

import (
	"fmt"
	"strings"

	"github.com/cedar-policy/cedar-go"
	"github.com/cedar-policy/cedar-go/types"
)

// actionGroups maps each tool to its Cedar action group (see authz.cedar header).
var actionGroups = map[string]string{
	"application.get":          "read",
	"application.next_actions": "read",
	"policy.evaluate":          "read",
	"approval.list":            "read",
	"task.list":                "read",
	"agent.run_list":           "read",

	"application.create":   "intake",
	"application.update":   "intake",
	"application.submit":   "intake",
	"application.withdraw": "intake",
	"party.add":            "intake",
	"party.update":         "intake",
	"consent.record":       "intake",
	"document.upload_url":  "intake",
	"document.attach":      "intake",
	"document.classify":    "intake",
	"document.extract":     "intake",
	"document.request":     "intake",
	"task.create":          "intake",
	"task.complete":        "intake",
	"task.remind":          "intake",

	"kyc.run":                 "verify",
	"sanctions.screen":        "verify",
	"credit.pull_report":      "verify",
	"bank.analyse_statements": "verify",
	"evidence.verify":         "verify",

	"decision.recommend": "recommend",
	"decision.refer":     "recommend",

	"offer.generate": "offer",
	"offer.send":     "offer",
}

// Actions that are deliberately outside any group and named directly in policies.
var ungroupedActions = map[string]bool{
	"decision.approve": true, "decision.decline": true, "offer.accept": true,
	"loan.handoff": true, "approval.respond": true,
	"agent.run_start": true, "agent.run_finish": true,
	"webhook.subscribe": true, "webhook.list": true, "webhook.remove": true,
}

// roleParents lets a role inherit another role's permissions.
var roleParents = map[string][]string{
	"senior_credit_approver": {"credit_approver"},
}

// Subject is a principal as Cedar sees it.
type Subject struct {
	ID                 string // e.g. user:officer.ana
	Kind               string // user | agent | svc | borrower
	TenantID           string
	Roles              []string
	ApprovalLimitMinor int64
}

// Resource is an application as Cedar sees it. ID may be "new" for create.
type Resource struct {
	ID                   string
	TenantID             string
	RequestedAmountMinor int64
	RecommendedBy        string // principal id, optional
	RecommendedFor       string // principal id, optional
	Borrower             string // principal id, optional
}

// Authorizer evaluates the pack's Cedar policies.
type Authorizer struct {
	policies *cedar.PolicySet
}

// NewAuthorizer parses Cedar policies.
func NewAuthorizer(name string, src []byte) (*Authorizer, error) {
	ps, err := cedar.NewPolicySetFromBytes(name, src)
	if err != nil {
		return nil, fmt.Errorf("cedar: %w", err)
	}
	return &Authorizer{policies: ps}, nil
}

// PolicyCount returns the number of parsed policies.
func (a *Authorizer) PolicyCount() int {
	n := 0
	for range a.policies.All() {
		n++
	}
	return n
}

// Decision is the result of an authorization check.
type Decision struct {
	Allowed bool     `json:"allowed"`
	Reasons []string `json:"reasons,omitempty"` // determining policy ids
	Errors  []string `json:"errors,omitempty"`
}

var kindToType = map[string]string{"user": "User", "agent": "Agent", "svc": "Service", "borrower": "Borrower"}

func uid(typ, id string) types.EntityUID {
	return types.NewEntityUID(types.EntityType("Tamarind::"+typ), types.String(id))
}

// principalUID converts "user:officer.ana" into Tamarind::User::"officer.ana".
func principalUID(id string) (types.EntityUID, bool) {
	kind, rest, ok := strings.Cut(id, ":")
	typ, known := kindToType[kind]
	if !ok || !known {
		return types.EntityUID{}, false
	}
	return uid(typ, rest), true
}

// Authorize checks whether subject may invoke tool on resource.
func (a *Authorizer) Authorize(s Subject, tool string, r Resource) Decision {
	entities := types.EntityMap{}
	add := func(u types.EntityUID, parents []types.EntityUID, attrs types.RecordMap) {
		entities[u] = types.Entity{UID: u, Parents: types.NewEntityUIDSet(parents...), Attributes: types.NewRecord(attrs)}
	}

	pUID, ok := principalUID(s.ID)
	if !ok {
		return Decision{Errors: []string{"unknown principal kind: " + s.ID}}
	}
	var roleUIDs []types.EntityUID
	for _, role := range s.Roles {
		ru := uid("Role", role)
		roleUIDs = append(roleUIDs, ru)
		var parents []types.EntityUID
		for _, p := range roleParents[role] {
			parents = append(parents, uid("Role", p))
			add(uid("Role", p), nil, nil)
		}
		add(ru, parents, nil)
	}
	pAttrs := types.RecordMap{"tenant": uid("Tenant", s.TenantID)}
	if s.ApprovalLimitMinor > 0 {
		pAttrs["approval_limit_minor"] = types.Long(s.ApprovalLimitMinor)
	}
	add(pUID, roleUIDs, pAttrs)

	aUID := uid("Action", tool)
	var aParents []types.EntityUID
	if g, ok := actionGroups[tool]; ok {
		aParents = append(aParents, uid("Action", g))
		add(uid("Action", g), nil, nil)
	}
	add(aUID, aParents, nil)

	rUID := uid("Application", r.ID)
	rAttrs := types.RecordMap{
		"tenant":                 uid("Tenant", r.TenantID),
		"requested_amount_minor": types.Long(r.RequestedAmountMinor),
	}
	for key, id := range map[string]string{"recommended_by": r.RecommendedBy, "recommended_for": r.RecommendedFor, "borrower": r.Borrower} {
		if id == "" {
			continue
		}
		if u, ok := principalUID(id); ok {
			rAttrs[types.String(key)] = u
		}
	}
	add(rUID, nil, rAttrs)

	dec, diag := cedar.Authorize(a.policies, entities, cedar.Request{
		Principal: pUID, Action: aUID, Resource: rUID, Context: types.NewRecord(nil),
	})
	out := Decision{Allowed: dec == cedar.Allow}
	for _, rs := range diag.Reasons {
		out.Reasons = append(out.Reasons, string(rs.PolicyID))
	}
	for _, e := range diag.Errors {
		out.Errors = append(out.Errors, e.String())
	}
	return out
}
