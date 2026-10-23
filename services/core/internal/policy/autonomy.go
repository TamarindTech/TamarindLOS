package policy

import "github.com/TamarindTech/TamarindLOS/services/core/internal/catalogue"

// Autonomy levels (Concept Paper 01 §5). "gate" behaves like L2 but always routes
// the call to a human approver.
var autonomyRank = map[string]int{"L0": 0, "L1": 1, "L2": 2, "gate": 2, "L3": 3, "L4": 4}

// AgentOutcome is what the gateway does with an agent's call at a given autonomy level.
type AgentOutcome string

const (
	// AgentExecute: the agent may act (L3, L4).
	AgentExecute AgentOutcome = "execute"
	// AgentApproval: the call becomes an approval request for a human (L2, gate).
	AgentApproval AgentOutcome = "approval"
	// AgentDeny: agents may only assist with this tool (L0, L1).
	AgentDeny AgentOutcome = "deny"
)

// EffectiveAutonomy returns the lowest of the tool's default (or the pack's setting),
// the tool's maximum, the tenant's setting and the actor's trust level. Empty values
// are ignored. On a tie between "gate" and "L2", "gate" wins.
func EffectiveAutonomy(t *catalogue.Tool, packSetting, tenantSetting, actorTrust string) string {
	base := t.DefaultAutonomy
	if packSetting != "" {
		base = packSetting
	}
	effective := base
	for _, c := range []string{t.MaxAutonomy, tenantSetting, actorTrust} {
		if c == "" {
			continue
		}
		if autonomyRank[c] < autonomyRank[effective] || (autonomyRank[c] == autonomyRank[effective] && c == "gate") {
			effective = c
		}
	}
	return effective
}

// OutcomeFor maps an autonomy level to what happens to an agent's call.
func OutcomeFor(level string) AgentOutcome {
	switch r := autonomyRank[level]; {
	case r >= 3:
		return AgentExecute
	case r == 2:
		return AgentApproval
	default:
		return AgentDeny
	}
}
