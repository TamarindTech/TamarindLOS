package policy

import (
	"fmt"
	"reflect"
	"sort"
	"sync"

	"github.com/google/cel-go/cel"

	"github.com/TamarindTech/TamarindLOS/services/core/internal/yamlx"
)

// Variables available to every CEL expression (rules, guards, preconditions).
var celVariables = []string{"application", "product", "checks", "metrics", "offer", "document", "now"}

// Exprs compiles and caches CEL expressions.
type Exprs struct {
	env   *cel.Env
	mu    sync.Mutex
	cache map[string]cel.Program
}

// NewExprs creates the shared CEL environment.
func NewExprs() (*Exprs, error) {
	opts := []cel.EnvOption{cel.CrossTypeNumericComparisons(true)}
	for _, v := range celVariables {
		opts = append(opts, cel.Variable(v, cel.DynType))
	}
	env, err := cel.NewEnv(opts...)
	if err != nil {
		return nil, err
	}
	return &Exprs{env: env, cache: map[string]cel.Program{}}, nil
}

// Compile compiles (or returns a cached) CEL program.
func (e *Exprs) Compile(expr string) (cel.Program, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if p, ok := e.cache[expr]; ok {
		return p, nil
	}
	ast, iss := e.env.Compile(expr)
	if iss.Err() != nil {
		return nil, iss.Err()
	}
	p, err := e.env.Program(ast)
	if err != nil {
		return nil, err
	}
	e.cache[expr] = p
	return p, nil
}

// EvalBool evaluates an expression that must return a boolean.
// Variables not supplied are bound to empty maps so missing data fails the expression
// (with an error) rather than panicking.
func (e *Exprs) EvalBool(expr string, vars map[string]any) (bool, error) {
	p, err := e.Compile(expr)
	if err != nil {
		return false, err
	}
	act := map[string]any{}
	for _, v := range celVariables {
		act[v] = map[string]any{}
	}
	for k, v := range vars {
		act[k] = v
	}
	out, _, err := p.Eval(act)
	if err != nil {
		return false, err
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("expression returned %s, not bool", reflect.TypeOf(out.Value()))
	}
	return b, nil
}

// --- Rules -------------------------------------------------------------------

// Rule is one CEL business rule.
type Rule struct {
	ID          string `yaml:"id"`
	Description string `yaml:"description"`
	Expression  string `yaml:"expression"`
	Outcome     string `yaml:"outcome"` // decline | refer
	ReasonCode  string `yaml:"reason_code"`
	Set         string `yaml:"-"`
}

// RuleResult is the evaluation of one rule.
type RuleResult struct {
	RuleID     string `json:"rule_id"`
	Set        string `json:"set"`
	Passed     bool   `json:"passed"`
	Outcome    string `json:"outcome,omitempty"`
	ReasonCode string `json:"reason_code,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Evaluation aggregates rule results into pass, refer or decline.
type Evaluation struct {
	Outcome string       `json:"outcome"`
	Results []RuleResult `json:"results"`
}

// Failed lists failed rule IDs, sorted.
func (ev Evaluation) Failed() []string {
	var ids []string
	for _, r := range ev.Results {
		if !r.Passed {
			ids = append(ids, r.RuleID)
		}
	}
	sort.Strings(ids)
	return ids
}

func loadRules(path string, exprs *Exprs) ([]*Rule, error) {
	var f struct {
		RuleSets map[string][]*Rule `yaml:"rule_sets"`
	}
	if err := yamlx.ReadNative(path, &f); err != nil {
		return nil, err
	}
	var rules []*Rule
	sets := make([]string, 0, len(f.RuleSets))
	for s := range f.RuleSets {
		sets = append(sets, s)
	}
	sort.Strings(sets)
	seen := map[string]bool{}
	for _, s := range sets {
		for _, r := range f.RuleSets[s] {
			if seen[r.ID] {
				return nil, fmt.Errorf("duplicate rule id %s", r.ID)
			}
			seen[r.ID] = true
			if r.Outcome != "decline" && r.Outcome != "refer" {
				return nil, fmt.Errorf("rule %s: outcome must be decline or refer", r.ID)
			}
			if _, err := exprs.Compile(r.Expression); err != nil {
				return nil, fmt.Errorf("rule %s: %w", r.ID, err)
			}
			r.Set = s
			rules = append(rules, r)
		}
	}
	return rules, nil
}

// EvaluateRules runs every rule. A rule that errors (for example because data is
// missing) counts as failed, with the error recorded.
func (p *Pack) EvaluateRules(vars map[string]any) Evaluation {
	ev := Evaluation{Outcome: "pass"}
	for _, r := range p.Rules {
		ok, err := p.Exprs.EvalBool(r.Expression, vars)
		res := RuleResult{RuleID: r.ID, Set: r.Set, Passed: ok && err == nil}
		if err != nil {
			res.Error = err.Error()
		}
		if !res.Passed {
			res.Outcome = r.Outcome
			res.ReasonCode = r.ReasonCode
			if r.Outcome == "decline" {
				ev.Outcome = "decline"
			} else if ev.Outcome == "pass" {
				ev.Outcome = "refer"
			}
		}
		ev.Results = append(ev.Results, res)
	}
	return ev
}

// TestCaseResult is the outcome of one tests.yaml case.
type TestCaseResult struct {
	Name       string
	Passed     bool
	Got, Want  string
	GotFailed  []string
	WantFailed []string
}

// RunRuleTests evaluates <pack>/tests.yaml.
func (p *Pack) RunRuleTests() ([]TestCaseResult, error) {
	var f struct {
		Cases []struct {
			Name   string         `yaml:"name"`
			Input  map[string]any `yaml:"input"`
			Expect struct {
				Outcome string   `yaml:"outcome"`
				Failed  []string `yaml:"failed"`
			} `yaml:"expect"`
		} `yaml:"cases"`
	}
	if err := yamlx.ReadNative(p.Dir+"/tests.yaml", &f); err != nil {
		return nil, err
	}
	var out []TestCaseResult
	for _, c := range f.Cases {
		ev := p.EvaluateRules(c.Input)
		want := append([]string{}, c.Expect.Failed...)
		sort.Strings(want)
		got := ev.Failed()
		out = append(out, TestCaseResult{
			Name: c.Name, Got: ev.Outcome, Want: c.Expect.Outcome, GotFailed: got, WantFailed: want,
			Passed: ev.Outcome == c.Expect.Outcome && fmt.Sprint(got) == fmt.Sprint(want),
		})
	}
	return out, nil
}
