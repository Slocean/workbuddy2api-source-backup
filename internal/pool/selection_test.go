package pool

import (
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func TestSelectionPolicyExpiryAndCostOrder(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "early"})
	p.Add(&auth.Auth{UID: "cheap"})
	now := time.Now()
	p.SetCreditsWithExpiries("early", 100, 0, []CreditExpiry{{Expiry: now.Add(48 * time.Hour), Remaining: 100}})
	p.SetCreditsWithExpiries("cheap", 100, 0, []CreditExpiry{{Expiry: now.Add(10 * 24 * time.Hour), Remaining: 100}})
	p.NoteModelCost("early", "m", 2, 1000)
	p.NoteModelCost("cheap", "m", 0.1, 1000)
	if !p.SetSelectionPolicy("expiry_then_cheapest") {
		t.Fatal("policy rejected")
	}
	if got := p.PickExcludingForRealm(nil, "m", "cn"); got == nil || got.UID != "early" {
		t.Fatalf("expiry first: got %v, want early", got)
	}
	p.SetSelectionPolicy("cheapest_then_expiry")
	if got := p.PickExcludingForRealm(nil, "m", "cn"); got == nil || got.UID != "cheap" {
		t.Fatalf("cost first: got %v, want cheap", got)
	}
}

func TestSelectionPolicyAdvancesAfterEarliestBucketSpent(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "first"})
	p.Add(&auth.Auth{UID: "second"})
	now := time.Now()
	p.SetCreditsWithExpiries("first", 20, 0, []CreditExpiry{
		{Expiry: now.Add(24 * time.Hour), Remaining: 10},
		{Expiry: now.Add(20 * 24 * time.Hour), Remaining: 10},
	})
	p.SetCreditsWithExpiries("second", 20, 0, []CreditExpiry{{Expiry: now.Add(10 * 24 * time.Hour), Remaining: 20}})
	p.SetSelectionPolicy("earliest_expiry")
	if got := p.PickExcludingForRealm(nil, "m", "cn"); got == nil || got.UID != "first" {
		t.Fatalf("before spend: got %v, want first", got)
	}
	p.NoteModelCost("first", "m", 10, 1000)
	if got := p.PickExcludingForRealm(nil, "m", "cn"); got == nil || got.UID != "second" {
		t.Fatalf("after spend: got %v, want second", got)
	}
}

func TestSelectionPolicyUnknownAndUnavailable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "known"})
	p.Add(&auth.Auth{UID: "unknown"})
	p.Add(&auth.Auth{UID: "global", Domain: "www.workbuddy.ai"})
	p.SetCreditsWithExpiries("known", 100, 0, []CreditExpiry{{Expiry: time.Now().Add(60 * 24 * time.Hour), Remaining: 100}})
	p.SetSelectionPolicy("expiry_then_cheapest")
	if got := p.PickExcludingForRealm(nil, "m", "cn"); got == nil || got.UID != "known" {
		t.Fatalf("unknown expiry or wrong realm took priority: %v", got)
	}
	p.Cooldown("known", CoolHard, time.Hour, "empty")
	if got := p.PickExcludingForRealm(nil, "m", "cn"); got == nil || got.UID != "unknown" {
		t.Fatalf("unavailable account not skipped: %v", got)
	}
}

func TestSelectionPolicyStaleExpiryIsUnknown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "stale"})
	p.Add(&auth.Auth{UID: "fresh"})
	now := time.Now()
	p.SetCreditsWithExpiries("stale", 100, 0, []CreditExpiry{{Expiry: now.Add(24 * time.Hour), Remaining: 100}})
	p.SetCreditsWithExpiries("fresh", 100, 0, []CreditExpiry{{Expiry: now.Add(48 * time.Hour), Remaining: 100}})
	p.byUID["stale"].creditObservedAt = now.Add(-2 * creditObservationTTL)
	p.SetSelectionPolicy("earliest_expiry")
	if got := p.PickExcludingForRealm(nil, "m", "cn"); got == nil || got.UID != "fresh" {
		t.Fatalf("stale expiry was treated as current: %v", got)
	}
}

func TestSelectionPolicyStickyFollowsBestRank(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "bound"})
	p.Add(&auth.Auth{UID: "other"})
	at := time.Now().Add(48 * time.Hour)
	p.SetCreditsWithExpiries("bound", 100, 0, []CreditExpiry{{Expiry: at, Remaining: 100}})
	p.SetCreditsWithExpiries("other", 100, 0, []CreditExpiry{{Expiry: at, Remaining: 100}})
	p.NoteModelCost("bound", "m", 1, 1000)
	p.NoteModelCost("other", "m", 1, 1000)
	p.SetSelectionPolicy("expiry_then_cheapest")
	if !p.PolicyAllowsSticky("bound", "m", "cn") {
		t.Fatal("equal-rank existing session should remain bound")
	}
	p.SetCreditsWithExpiries("other", 100, 0, []CreditExpiry{{Expiry: at.Add(-time.Hour), Remaining: 100}})
	if p.PolicyAllowsSticky("bound", "m", "cn") {
		t.Fatal("better expiry should move an existing session")
	}
}

func TestSelectionPolicyExpiryPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	p := New(path)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsWithExpiries("u1", 100, 0, []CreditExpiry{{Expiry: time.Now().Add(48 * time.Hour), Remaining: 100}})
	p.Flush()
	p.Close()
	p2 := New(path)
	defer p2.Close()
	p2.Add(&auth.Auth{UID: "u1"})
	st, _ := p2.Status("u1")
	if len(st.CreditExpiries) != 1 || st.CreditExpiries[0].Remaining != 100 || st.CreditObservedAt.IsZero() {
		t.Fatalf("credit expiry snapshot was lost on restart: %+v", st)
	}
}
