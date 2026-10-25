// Package policy loads a policy pack (product, state machine, CEL rules, Cedar
// authorization) and evaluates it. See Spec 05 and ADR-0004.
package policy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/TamarindTech/TamarindLOS/services/core/internal/catalogue"
	"github.com/TamarindTech/TamarindLOS/services/core/internal/yamlx"
)

// Range is an inclusive integer range.
type Range struct {
	Min int64 `json:"min"`
	Max int64 `json:"max"`
}

// Product is the product section of pack.yaml.
type Product struct {
	Code              string `json:"code"`
	Name              string `json:"name"`
	Currency          string `json:"currency"`
	AmountMinor       Range  `json:"amount_minor"`
	TermMonths        Range  `json:"term_months"`
	Repayment         string `json:"repayment"`
	OfferValidityDays int    `json:"offer_validity_days"`
	Pricing           struct {
		Method        string         `json:"method"`
		APRBpsByGrade map[string]int `json:"apr_bps_by_grade"`
	} `json:"pricing"`
}

// ReferenceAPRBps is the rate used to estimate repayments before pricing is decided
// (the middle risk grade, B, when present).
func (p Product) ReferenceAPRBps() int {
	if r, ok := p.Pricing.APRBpsByGrade["B"]; ok {
		return r
	}
	for _, r := range p.Pricing.APRBpsByGrade {
		return r
	}
	return 2000
}

// PartyRequirement is one entry of parties.required.
type PartyRequirement struct {
	Role string `json:"role"`
	Kind string `json:"kind"`
	Min  int    `json:"min"`
	Max  int    `json:"max"`
}

// DocumentRequirement is one entry of documents.
type DocumentRequirement struct {
	Type         string     `json:"type"`
	Party        StringList `json:"party"` // role, list of roles, or "all"
	Required     bool       `json:"required"`
	RequiredWhen string     `json:"required_when"` // CEL over `application`; makes the document required when true
	MinMonths    int        `json:"min_months"`
	MinYears     int        `json:"min_years"`
}

// AppliesTo reports whether the requirement covers a party role.
func (r DocumentRequirement) AppliesTo(role string) bool {
	for _, p := range r.Party {
		if p == "all" || p == role {
			return true
		}
	}
	return false
}

// CheckRequirement is one entry of checks: which check must run for which parties.
type CheckRequirement struct {
	Type      string     `json:"type"`   // kyb | kyc | sanctions | credit_bureau | bank_analysis
	Report    string     `json:"report"` // for credit_bureau: commercial | consumer
	Party     StringList `json:"party"`  // role, list of roles, or "all"
	Mandatory bool       `json:"mandatory"`
	Consent   string     `json:"consent"`
}

// AppliesTo reports whether the requirement covers a party role.
func (r CheckRequirement) AppliesTo(role string) bool {
	for _, p := range r.Party {
		if p == "all" || p == role {
			return true
		}
	}
	return false
}

// Authority is an approval limit for a role, in minor units.
type Authority struct {
	Role       string `json:"role"`
	LimitMinor int64  `json:"limit_minor"`
}

// Pack is a loaded policy pack.
type Pack struct {
	Name         string  `json:"pack"`
	Version      string  `json:"version"`
	Status       string  `json:"status"`
	Jurisdiction string  `json:"jurisdiction"`
	Product      Product `json:"product"`
	Parties      struct {
		Required []PartyRequirement `json:"required"`
	} `json:"parties"`
	Documents         []DocumentRequirement `json:"documents"`
	Checks            []CheckRequirement    `json:"checks"`
	RulesFile         string                `json:"rules"`
	StateMachineFile  string                `json:"state_machine"`
	AuthorizationFile string                `json:"authorization"`
	Autonomy          map[string]string     `json:"autonomy"`
	ApprovalAuthority []Authority           `json:"approval_authority"`
	Evidence          struct {
		// Agents may mark evidence verified only at or above this extraction confidence.
		MinConfidenceAutoVerify float64 `json:"min_confidence_auto_verify"`
	} `json:"evidence"`

	// ProductData is the raw product section, exposed to CEL as `product`.
	ProductData map[string]any `json:"-"`

	Rules        []*Rule      `json:"-"`
	StateMachine StateMachine `json:"-"`
	Authz        *Authorizer  `json:"-"`
	Exprs        *Exprs       `json:"-"`
	Dir          string       `json:"-"`
}

// LoadPack loads pack.yaml and the files it references from dir.
func LoadPack(dir string) (*Pack, error) {
	p := &Pack{Dir: dir}
	if err := yamlx.DecodeFile(filepath.Join(dir, "pack.yaml"), p); err != nil {
		return nil, err
	}
	var raw struct {
		Product map[string]any `yaml:"product"`
	}
	if err := yamlx.ReadNative(filepath.Join(dir, "pack.yaml"), &raw); err != nil {
		return nil, err
	}
	p.ProductData = raw.Product

	exprs, err := NewExprs()
	if err != nil {
		return nil, err
	}
	p.Exprs = exprs

	if p.Rules, err = loadRules(filepath.Join(dir, p.RulesFile), exprs); err != nil {
		return nil, err
	}
	if err := yamlx.DecodeFile(filepath.Join(dir, p.StateMachineFile), &p.StateMachine); err != nil {
		return nil, err
	}
	cedarSrc, err := os.ReadFile(filepath.Join(dir, p.AuthorizationFile))
	if err != nil {
		return nil, err
	}
	if p.Authz, err = NewAuthorizer(p.AuthorizationFile, cedarSrc); err != nil {
		return nil, err
	}
	return p, nil
}

// ApprovalLimit returns the highest approval limit granted by any of the roles.
func (p *Pack) ApprovalLimit(roles []string) int64 {
	var limit int64
	for _, a := range p.ApprovalAuthority {
		for _, r := range roles {
			if r == a.Role && a.LimitMinor > limit {
				limit = a.LimitMinor
			}
		}
	}
	return limit
}

// Validate cross-checks the pack against the tool catalogue. It returns every problem found.
func (p *Pack) Validate(cat *catalogue.Catalogue) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	states := map[string]bool{}
	for _, s := range p.StateMachine.States {
		states[s.Name] = true
	}
	// State machine transitions reference known states and tools; guards compile.
	smByTool := map[string][]SMTransition{}
	for i, t := range p.StateMachine.Transitions {
		for _, f := range t.From {
			if !states[f] {
				add("state machine transition %d: unknown from-state %q", i, f)
			}
		}
		if !states[t.To] {
			add("state machine transition %d: unknown to-state %q", i, t.To)
		}
		switch t.By {
		case "tool":
			if _, ok := cat.Tools[t.Tool]; !ok {
				add("state machine transition %d: unknown tool %q", i, t.Tool)
			}
			smByTool[t.Tool] = append(smByTool[t.Tool], t)
		case "system":
			if _, err := p.Exprs.Compile(t.Guard); err != nil {
				add("state machine transition %d: guard does not compile: %v", i, err)
			}
		default:
			add("state machine transition %d: 'by' must be tool or system", i)
		}
	}
	// Tool transitions in the catalogue match the pack's state machine.
	for _, name := range cat.Names {
		t := cat.Tools[name]
		for _, pc := range t.Preconditions {
			if _, err := p.Exprs.Compile(pc); err != nil {
				add("tool %s: precondition %q does not compile: %v", name, pc, err)
			}
		}
		if t.Transition == nil {
			continue
		}
		sm := smByTool[name]
		if len(sm) != 1 {
			add("tool %s: expected exactly one state-machine transition, found %d", name, len(sm))
			continue
		}
		if !sameSet(sm[0].From, t.Transition.From) || sm[0].To != t.Transition.To {
			add("tool %s: catalogue transition %v -> %s differs from state machine %v -> %s",
				name, t.Transition.From, t.Transition.To, sm[0].From, sm[0].To)
		}
	}
	for _, d := range p.Documents {
		if d.RequiredWhen != "" {
			if _, err := p.Exprs.Compile(d.RequiredWhen); err != nil {
				add("document %s: required_when does not compile: %v", d.Type, err)
			}
		}
		if len(d.Party) == 0 {
			add("document %s: party is required", d.Type)
		}
	}
	for tool, level := range p.Autonomy {
		if _, ok := cat.Tools[tool]; !ok {
			add("autonomy: unknown tool %q", tool)
		}
		if _, ok := autonomyRank[level]; !ok {
			add("autonomy: tool %s has invalid level %q", tool, level)
		}
	}
	for _, name := range cat.Names {
		if _, ok := actionGroups[name]; !ok {
			if !ungroupedActions[name] {
				add("tool %s has no Cedar action group", name)
			}
		}
	}
	return problems
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	return strings.Join(x, ",") == strings.Join(y, ",")
}

// --- State machine -----------------------------------------------------------

// StateMachine is state-machine.yaml.
type StateMachine struct {
	Version     string         `json:"version"`
	States      []SMState      `json:"states"`
	Transitions []SMTransition `json:"transitions"`
}

// SMState is a state in the state machine.
type SMState struct {
	Name     string `json:"name"`
	Terminal bool   `json:"terminal"`
}

// SMTransition is a transition; From accepts a single state or a list.
type SMTransition struct {
	From  StringList `json:"from"`
	To    string     `json:"to"`
	By    string     `json:"by"`
	Tool  string     `json:"tool"`
	Guard string     `json:"guard"`
}

// StringList decodes either "x" or ["x", "y"].
type StringList []string

// UnmarshalJSON implements json.Unmarshaler.
func (s *StringList) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*s = []string{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*s = many
	return nil
}

// SystemTransitions returns the automatic transitions out of a state.
func (sm StateMachine) SystemTransitions(from string) []SMTransition {
	var out []SMTransition
	for _, t := range sm.Transitions {
		if t.By != "system" {
			continue
		}
		for _, f := range t.From {
			if f == from {
				out = append(out, t)
			}
		}
	}
	return out
}
