package policy

import (
	"fmt"
	"sort"
)

// RiskGrading is the pack's scorecard (pack.yaml risk_grading). Each factor whose
// CEL condition holds adds its points; the grade is the first band, from the top,
// whose minimum the total reaches. The grade sets the price through
// product.pricing.apr_bps_by_grade.
type RiskGrading struct {
	Version string       `json:"version"`
	Factors []RiskFactor `json:"factors"`
	Bands   []RiskBand   `json:"bands"`
}

// RiskFactor is one scorecard line.
type RiskFactor struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	When        string `json:"when"`
	Points      int    `json:"points"`
	// Group marks alternatives (for example two DSCR tiers): at most one should hold,
	// and only the best of them counts towards the maximum.
	Group string `json:"group,omitempty"`
}

// RiskBand maps a points total to a grade.
type RiskBand struct {
	Grade     string `json:"grade"`
	MinPoints int    `json:"min_points"`
}

// FactorResult is how one factor scored. A factor whose data is missing scores
// nothing and is listed in RiskGrade.Missing, so gaps never improve a grade.
type FactorResult struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Points      int    `json:"points"`
	Awarded     bool   `json:"awarded"`
	Missing     bool   `json:"missing,omitempty"`
}

// RiskGrade is the scorecard outcome for an application.
type RiskGrade struct {
	Grade     string         `json:"grade"`
	Points    int            `json:"points"`
	MaxPoints int            `json:"max_points"`
	APRBps    int            `json:"apr_bps"`
	Factors   []FactorResult `json:"factors"`
	Missing   []string       `json:"missing"`
	Scorecard string         `json:"scorecard"` // pack name and scorecard version
}

// GradeRisk scores an application. It returns nil when the pack has no scorecard.
func (p *Pack) GradeRisk(vars map[string]any) *RiskGrade {
	rg := p.RiskGrading
	if rg == nil || len(rg.Bands) == 0 {
		return nil
	}
	out := &RiskGrade{Factors: []FactorResult{}, Missing: []string{}, Scorecard: p.Name + "/risk-grading@" + rg.Version}
	for _, f := range rg.Factors {
		ok, err := p.Exprs.EvalBool(f.When, vars)
		r := FactorResult{ID: f.ID, Description: f.Description, Awarded: ok && err == nil, Missing: err != nil}
		if r.Awarded {
			r.Points = f.Points
			out.Points += f.Points
		}
		if r.Missing {
			out.Missing = append(out.Missing, f.ID)
		}
		out.Factors = append(out.Factors, r)
	}
	out.MaxPoints = rg.maxPoints()
	for _, b := range rg.sortedBands() {
		if out.Points >= b.MinPoints {
			out.Grade = b.Grade
			break
		}
	}
	out.APRBps = p.Product.Pricing.APRBpsByGrade[out.Grade]
	return out
}

func (rg *RiskGrading) sortedBands() []RiskBand {
	bands := append([]RiskBand{}, rg.Bands...)
	sort.SliceStable(bands, func(i, j int) bool { return bands[i].MinPoints > bands[j].MinPoints })
	return bands
}

// maxPoints is the best achievable total, counting only the best factor of each group.
func (rg *RiskGrading) maxPoints() int {
	n, best := 0, map[string]int{}
	for _, f := range rg.Factors {
		if f.Group == "" {
			n += f.Points
		} else if f.Points > best[f.Group] {
			best[f.Group] = f.Points
		}
	}
	for _, v := range best {
		n += v
	}
	return n
}

// LowestAPRBps is the cheapest price in the grade table.
func (p Product) LowestAPRBps() int {
	low := 0
	for _, r := range p.Pricing.APRBpsByGrade {
		if low == 0 || r < low {
			low = r
		}
	}
	return low
}

// validateRiskGrading checks the scorecard: factors compile, ids are unique, every band's
// grade has a price and some band starts at zero so every application gets a grade.
func (p *Pack) validateRiskGrading() []string {
	rg := p.RiskGrading
	if rg == nil {
		return nil
	}
	var problems []string
	seen := map[string]bool{}
	for _, f := range rg.Factors {
		if seen[f.ID] {
			problems = append(problems, fmt.Sprintf("risk_grading: duplicate factor %s", f.ID))
		}
		seen[f.ID] = true
		if _, err := p.Exprs.Compile(f.When); err != nil {
			problems = append(problems, fmt.Sprintf("risk_grading: factor %s does not compile: %v", f.ID, err))
		}
		if f.Points <= 0 {
			problems = append(problems, fmt.Sprintf("risk_grading: factor %s needs positive points", f.ID))
		}
	}
	zero := false
	for _, b := range rg.Bands {
		if _, ok := p.Product.Pricing.APRBpsByGrade[b.Grade]; !ok {
			problems = append(problems, fmt.Sprintf("risk_grading: grade %s has no price in product.pricing.apr_bps_by_grade", b.Grade))
		}
		zero = zero || b.MinPoints <= 0
	}
	if len(rg.Bands) == 0 || !zero {
		problems = append(problems, "risk_grading: needs a band with min_points 0 so every application is graded")
	}
	return problems
}
