// Package auth resolves callers to principals.
//
// Phase 0 uses a static file of development bearer tokens. It is NOT suitable for
// production: replace it with OIDC for people and OAuth 2.1 client credentials for
// agents and systems before handling real data.
package auth

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"

	"github.com/TamarindTech/TamarindLOS/services/core/internal/yamlx"
)

// Principal is an authenticated caller.
type Principal struct {
	ID         string   `json:"id"` // user:..., agent:..., svc:..., borrower:...
	Name       string   `json:"name"`
	Tenant     string   `json:"tenant"` // tenant slug
	Roles      []string `json:"roles"`
	Scopes     []string `json:"scopes"`
	TrustLevel string   `json:"trust_level,omitempty"` // agents only, e.g. L3
	Token      string   `json:"token"`
}

// Kind is the principal type prefix: user, agent, svc or borrower.
func (p *Principal) Kind() string {
	k, _, _ := strings.Cut(p.ID, ":")
	return k
}

// IsAgent reports whether the principal is an AI agent.
func (p *Principal) IsAgent() bool { return p.Kind() == "agent" }

// HasScopes reports whether the principal holds every required scope.
func (p *Principal) HasScopes(required []string) (bool, []string) {
	var missing []string
	for _, r := range required {
		ok := false
		for _, s := range p.Scopes {
			if s == "*" || s == r {
				ok = true
				break
			}
		}
		if !ok {
			missing = append(missing, r)
		}
	}
	return len(missing) == 0, missing
}

// Tenant is a tenant declared in the principals file.
type Tenant struct {
	Name string `json:"name"`
}

// Registry holds the known principals and tenants.
type Registry struct {
	Tenants    map[string]Tenant `json:"tenants"`
	Principals []*Principal      `json:"principals"`
	byID       map[string]*Principal
}

// LoadRegistry reads a principals YAML file.
func LoadRegistry(path string) (*Registry, error) {
	r := &Registry{}
	if err := yamlx.DecodeFile(path, r); err != nil {
		return nil, err
	}
	r.byID = map[string]*Principal{}
	for _, p := range r.Principals {
		if _, ok := r.Tenants[p.Tenant]; !ok {
			return nil, fmt.Errorf("principal %s: unknown tenant %q", p.ID, p.Tenant)
		}
		switch p.Kind() {
		case "user", "agent", "svc", "borrower":
		default:
			return nil, fmt.Errorf("principal %s: id must start with user:, agent:, svc: or borrower:", p.ID)
		}
		if p.Token == "" {
			return nil, fmt.Errorf("principal %s: missing token", p.ID)
		}
		if _, dup := r.byID[p.ID]; dup {
			return nil, fmt.Errorf("duplicate principal %s", p.ID)
		}
		r.byID[p.ID] = p
	}
	return r, nil
}

// ByID returns the principal with the given id.
func (r *Registry) ByID(id string) (*Principal, bool) {
	p, ok := r.byID[id]
	return p, ok
}

// Authenticate resolves the bearer token on a request.
func (r *Registry) Authenticate(req *http.Request) (*Principal, bool) {
	h := req.Header.Get("Authorization")
	token, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || token == "" {
		return nil, false
	}
	for _, p := range r.Principals {
		if subtle.ConstantTimeCompare([]byte(p.Token), []byte(token)) == 1 {
			return p, true
		}
	}
	return nil, false
}
