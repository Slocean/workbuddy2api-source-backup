package pool

import (
	"testing"
	"time"
	"workbuddy2api/internal/auth"
)

func TestSelectionScopeCombinesWithOrdering(t *testing.T) {
	for _, policy := range []string{"balanced", "cheapest", "earliest_expiry", "expiry_then_cheapest", "cheapest_then_expiry"} {
		for _, scope := range []string{"prefer_global", "prefer_cn", "all"} {
			t.Run(policy+"/"+scope, func(t *testing.T) {
				p := realmPool(t)
				p.SetSelectionConfig(policy, scope)
				now := time.Now()
				for _, uid := range []string{"cn1", "cn2", "g1", "g2"} {
					days, cost := 10, 10.0
					if uid == "cn1" {
						days, cost = 1, 1
					}
					p.SetCreditsWithExpiries(uid, 100, 100, []CreditExpiry{{Expiry: now.Add(time.Duration(days) * 24 * time.Hour), Remaining: 100}})
					p.NoteModelCost(uid, "m", cost, 1000)
				}
				got := p.PickForScope(nil, "m", "global", "", nil)
				if got == nil {
					t.Fatal("no account")
				}
				if scope == "prefer_global" && got.Realm() != "global" {
					t.Fatalf("wrong pool: %s", got.UID)
				}
				if scope == "prefer_cn" && got.Realm() != "cn" {
					t.Fatalf("wrong pool: %s", got.UID)
				}
				if scope == "all" && policy != "balanced" && got.UID != "cn1" {
					t.Fatalf("combined ordering: %s", got.UID)
				}
			})
		}
	}
}
func TestScopeFallbackStickyAndPermissions(t *testing.T) {
	p := realmPool(t)
	p.SetSelectionConfig("cheapest", "prefer_global")
	p.NoteModelCost("cn1", "m", 1, 1000)
	p.NoteModelCost("g1", "m", 10, 1000)
	if p.ScopeAllowsSticky("cn1", "m", "cn", "", nil) {
		t.Fatal("sticky bypassed preferred pool")
	}
	got := p.PickForScope(nil, "m", "global", "cn", nil)
	if got == nil || got.Realm() != "cn" {
		t.Fatal("hard permission bypass")
	}
	allow := func(a *auth.Auth) bool { return a.UID == "cn1" }
	if !p.ScopeAllowsSticky("cn1", "m", "global", "", allow) {
		t.Fatal("compatible fallback binding rejected")
	}
	if len(p.AvailableUIDsForScope("m", "global")) != 4 {
		t.Fatal("availability erased fallback candidates")
	}
	got = p.PickForScope(nil, "m", "global", "", allow)
	if got == nil || got.UID != "cn1" {
		t.Fatal("compatibility bypass")
	}
	p.mu.Lock()
	for _, uid := range []string{"g1", "g2"} {
		p.byUID[uid].until = time.Now().Add(time.Minute)
		p.byUID[uid].coolKind = CoolSoft
	}
	p.mu.Unlock()
	got = p.PickForScope(nil, "m", "global", "", nil)
	if got == nil || got.Realm() != "cn" {
		t.Fatal("cooling preferred pool beat healthy fallback")
	}
	if !p.ScopeAllowsSticky("cn1", "m", "global", "", nil) {
		t.Fatal("valid fallback sticky lost")
	}
	got = p.PickForScope(map[string]bool{"cn1": true, "cn2": true}, "m", "global", "", nil)
	if got == nil || got.Realm() != "global" {
		t.Fatal("cooling fallback missing")
	}
	p.BlockModelBackoff("g1", "m", "11102")
	p.BlockModelBackoff("g2", "m", "11102")
	if got = p.PickForScope(map[string]bool{"cn1": true, "cn2": true}, "m", "global", "", nil); got != nil {
		t.Fatal("fallback undid model negative cache")
	}
}
func TestScopeLegacyAndLiveChange(t *testing.T) {
	p := realmPool(t)
	if got := p.PickForScope(nil, "m", "cn", "", nil); got == nil || got.Realm() != "cn" {
		t.Fatal("legacy realm changed")
	}
	p.SetSelectionScope("prefer_global")
	if got := p.PickForScope(nil, "m", "cn", "", nil); got == nil || got.Realm() != "global" {
		t.Fatal("scope not hot")
	}
	p.SetSelectionScope("prefer_cn")
	if got := p.PickForScope(map[string]bool{"cn1": true, "cn2": true}, "m", "cn", "", nil); got == nil || got.Realm() != "global" {
		t.Fatal("retry fallback missing")
	}
}
