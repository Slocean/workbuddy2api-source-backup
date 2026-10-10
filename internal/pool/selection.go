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

// creditMicroPerUnit 小数费用定点累计精度：1 credit = 1e6 微分（定点整数加减，
// 避免 float 连加的边界漂移；单笔舍入误差 ≤0.5e-6 credit）。
const creditMicroPerUnit = int64(1_000_000)

// maxSafeCredit microFromCredit 的输入上界：×1e6 后仍远小于 int64 上限与 float64
// 精确整数范围（2^53），巨值/+Inf 钳到该上界，杜绝 int64 溢出成负绕过扣减钳制。
const maxSafeCredit = 1e9

// microFromCredit 一笔消耗折成微分（四舍五入到 1e-6；NaN/负值拒绝为 0）。
func microFromCredit(credit float64) int64 {
	if !(credit >= 0) { // NaN 不满足任何比较，与负值一并拒绝
		return 0
	}
	if credit > maxSafeCredit {
		credit = maxSafeCredit
	}
	return int64(credit*float64(creditMicroPerUnit) + 0.5)
}

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
	// 权威快照覆盖估算：小数费用累计器是旧快照上的内插残余，一并弃用。
	e.creditPendingMicro = 0
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
