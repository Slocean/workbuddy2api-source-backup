package pool

import (
	"sort"
	"time"
	"workbuddy2api/internal/auth"
)

func ValidSelectionScope(scope string) bool {
	return scope == "" || scope == "prefer_global" || scope == "prefer_cn" || scope == "all"
}
func (p *Pool) SetSelectionScope(scope string) bool {
	if !ValidSelectionScope(scope) {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.selectionScope = scope
	return true
}
func (p *Pool) SelectionScope() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.selectionScope
}

// Scope filters precede ordering. An empty scope preserves prefix-based routing.
func (p *Pool) scopeCandidates(cands []*entry, requested, hard string, allow func(*auth.Auth) bool) []*entry {
	out := make([]*entry, 0, len(cands))
	for _, e := range cands {
		realm := e.a.Realm()
		if hard != "" && realm != hard {
			continue
		}
		if p.selectionScope == "" && requested != "" && realm != requested {
			continue
		}
		if allow != nil && !allow(e.a) {
			continue
		}
		out = append(out, e)
	}
	preferred := ""
	switch p.selectionScope {
	case "prefer_global":
		preferred = "global"
	case "prefer_cn":
		preferred = "cn"
	}
	if preferred != "" {
		first := make([]*entry, 0, len(out))
		for _, e := range out {
			if e.a.Realm() == preferred {
				first = append(first, e)
			}
		}
		if len(first) > 0 {
			return first
		}
	}
	return out
}

func (p *Pool) PickForScope(tried map[string]bool, model, requested, hard string, allow func(*auth.Auth) bool) *auth.Auth {
	return p.pickFiltered(tried, model, "", requested, hard, allow, true)
}
func (p *Pool) AvailableUIDsForScope(model, requested string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	var cands []*entry
	for _, e := range p.byUID {
		if e.healthyForModel(now, model) && !p.inFlightFull(e) {
			cands = append(cands, e)
		}
	}
	// Sticky validation must retain fallback accounts until request compatibility is checked.
	if p.selectionScope == "" {
		cands = p.scopeCandidates(cands, requested, "", nil)
	}
	out := make([]string, 0, len(cands))
	for _, e := range cands {
		out = append(out, e.a.UID)
	}
	sort.Strings(out)
	return out
}

// Both settings change under the same lock after persistence succeeds.
func (p *Pool) SetSelectionConfig(policy, scope string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.selectionPolicy, p.selectionScope = policy, scope
}
