package scheduler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

func TestRefreshGlobalCreditsUsesBillingWithoutCheckin(t *testing.T) {
	var paths []string
	expiry := time.Now().Add(3 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if !strings.Contains(r.URL.Path, "get-user-resource") {
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleEndTime":"` + expiry + `","CycleCapacitySize":100,"CycleCapacityRemain":80}]}}}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "c1", AccessToken: "at"})
	up := &upstream.Client{HTTP: srv.Client(), BillingBaseGlobal: srv.URL, GlobalEnabled: true}
	s := New(Config{Pool: p, Upstream: up})
	s.RefreshGlobalCredits()
	if len(paths) != 1 {
		t.Fatalf("billing calls=%d, want one global-only call", len(paths))
	}
	st, _ := p.Status("g1")
	if st.Credits != 80 || len(st.CreditExpiries) != 1 || st.CreditExpiries[0].Remaining != 80 {
		t.Fatalf("global expiry snapshot missing: %+v", st)
	}
}
