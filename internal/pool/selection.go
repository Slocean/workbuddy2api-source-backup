package pool

import (
	"math"
	"sort"
	"time"
	"workbuddy2api/internal/auth"
)

// CreditExpiry is a package balance with a known, future expiration time.
type CreditExpiry struct {
	Expiry    time.Time `json:"expiry"`
	Remaining int64     `json:"remaining"`
}

const creditObservationTTL = 24 * time.Hour

// SetSelectionPolicy switches the account ordering used after health, realm,
// model cooldown, retry and in-flight filtering. The default remains balanced.
func (p *Pool) SetSelectionPolicy(policy string) bool {
	switch policy {
	case "balanced", "cheapest", "earliest_expiry", "expiry_then_cheapest", "cheapest_then_expiry":
	default:
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.selectionPolicy = policy
	return true
}

func (p *Pool) SelectionPolicy() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.selectionPolicy == "" {
		return "balanced"
	}
	return p.selectionPolicy
}

// SetCreditsWithExpiries replaces the authoritative balance snapshot. A failed
// billing query must not call this method; its previous snapshot will age out.
func (p *Pool) SetCreditsWithExpiries(uid string, credits, expiring int64, packages []CreditExpiry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	if credits < 0 {
		credits = 0
	}
	if expiring < 0 {
		expiring = 0
	}
	if expiring > credits {
		expiring = credits
	}
	now := time.Now()
	clean := make([]CreditExpiry, 0, len(packages))
	for _, item := range packages {
		if item.Remaining <= 0 || !item.Expiry.After(now) {
			continue
		}
		clean = append(clean, item)
	}
	sort.Slice(clean, func(i, j int) bool { return clean[i].Expiry.Before(clean[j].Expiry) })
	var known int64
	valid := clean[:0]
	for _, item := range clean {
		if known >= credits {
			break
		}
		if item.Remaining > credits-known {
			item.Remaining = credits - known
		}
		known += item.Remaining
		valid = append(valid, item)
	}
	e.credits = credits
	e.creditsExpiring = expiring
	e.creditExpiries = valid
	e.creditObservedAt = now
	p.dirty.Store(true)
}

func (e *entry) earliestCreditExpiry(now time.Time) time.Time {
	if e.creditObservedAt.IsZero() || now.Sub(e.creditObservedAt) > creditObservationTTL || e.credits <= 0 {
		return time.Time{}
	}
	for _, item := range e.creditExpiries {
		if item.Remaining > 0 && item.Expiry.After(now) {
			return item.Expiry
		}
	}
	return time.Time{}
}

func (e *entry) spendCreditExpiries(amount int64) {
	now := time.Now()
	for i := range e.creditExpiries {
		if amount <= 0 {
			break
		}
		if !e.creditExpiries[i].Expiry.After(now) {
			continue
		}
		n := amount
		if n > e.creditExpiries[i].Remaining {
			n = e.creditExpiries[i].Remaining
		}
		e.creditExpiries[i].Remaining -= n
		amount -= n
	}
}

func (p *Pool) preferredAccount(cands []*entry, model string, now time.Time) *entry {
	policy := p.selectionPolicy
	if policy == "" || policy == "balanced" || len(cands) == 0 {
		return nil
	}
	cost := func(e *entry) float64 {
		if mc, ok := e.modelCostOf(model, now); ok {
			return mc.CostPer1k
		}
		return math.Inf(1)
	}
	expiry := func(e *entry) time.Time {
		return e.earliestCreditExpiry(now)
	}
	lessExpiry := func(a, b *entry) int {
		x, y := expiry(a), expiry(b)
		if x.IsZero() && y.IsZero() {
			return 0
		}
		if x.IsZero() {
			return 1
		}
		if y.IsZero() || x.Before(y) {
			return -1
		}
		if x.After(y) {
			return 1
		}
		return 0
	}
	lessCost := func(a, b *entry) int {
		x, y := cost(a), cost(b)
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
		return 0
	}
	best := cands[0]
	for _, e := range cands[1:] {
		cmp := 0
		switch policy {
		case "cheapest", "cheapest_then_expiry":
			cmp = lessCost(e, best)
			if cmp == 0 && policy == "cheapest_then_expiry" {
				cmp = lessExpiry(e, best)
			}
		case "earliest_expiry", "expiry_then_cheapest":
			cmp = lessExpiry(e, best)
			if cmp == 0 && policy == "expiry_then_cheapest" {
				cmp = lessCost(e, best)
			}
		}
		if cmp < 0 || (cmp == 0 && (e.usedSeq < best.usedSeq || (e.usedSeq == best.usedSeq && e.a.UID < best.a.UID))) {
			best = e
		}
	}
	return best
}

// PolicyAllowsSticky keeps an existing session binding while it is tied for
// the best policy rank. A better account causes the caller to select again.
func (p *Pool) PolicyAllowsSticky(uid, model, realm string) bool {
	return p.policyAllowsSticky(uid, model, realm, "", "", nil, false)
}
func (p *Pool) ScopeAllowsSticky(uid, model, requested, hard string, allow func(*auth.Auth) bool) bool {
	return p.policyAllowsSticky(uid, model, "", requested, hard, allow, true)
}
func (p *Pool) policyAllowsSticky(uid, model, realm, requested, hard string, allow func(*auth.Auth) bool, scoped bool) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	bound, ok := p.byUID[uid]
	if !ok || !bound.healthyForModel(now, model) || p.inFlightFull(bound) || (realm != "" && bound.a.Realm() != realm) {
		return false
	}
	var cands []*entry
	for _, e := range p.byUID {
		if e.healthyForModel(now, model) && !p.inFlightFull(e) && (realm == "" || e.a.Realm() == realm) {
			cands = append(cands, e)
		}
	}
	if scoped {
		cands = p.scopeCandidates(cands, requested, hard, allow)
		found := false
		for _, e := range cands {
			if e == bound {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	best := p.preferredAccount(cands, model, now)
	if best == nil {
		return true
	}
	sameExpiry := bound.earliestCreditExpiry(now).Equal(best.earliestCreditExpiry(now))
	boundCost, boundKnown := bound.modelCostOf(model, now)
	bestCost, bestKnown := best.modelCostOf(model, now)
	sameCost := boundKnown == bestKnown && (!boundKnown || boundCost.CostPer1k == bestCost.CostPer1k)
	switch p.selectionPolicy {
	case "cheapest":
		return sameCost
	case "earliest_expiry":
		return sameExpiry
	case "cheapest_then_expiry", "expiry_then_cheapest":
		return sameExpiry && sameCost
	default:
		return true
	}
}
