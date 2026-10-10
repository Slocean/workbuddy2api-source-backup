package scheduler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// TestRefreshCreditsCoversBothRealmsWithoutCheckin 余额刷新覆盖全部有 token 的
// 未禁用账号（CN 与 global 同等）：只读 get-user-resource + 权威快照重置，
// 绝不触发签到路径——CN 的本地估算余额在两次签到之间也由官方数据校正。
func TestRefreshCreditsCoversBothRealmsWithoutCheckin(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	expiry := time.Now().Add(3 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
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
	up := &upstream.Client{HTTP: srv.Client(), BillingBaseCN: srv.URL, BillingBaseGlobal: srv.URL, GlobalEnabled: true}
	s := New(Config{Pool: p, Upstream: up})
	s.RefreshCredits()
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 2 {
		t.Fatalf("billing calls=%d, want exactly one per account (CN+global): %v", len(paths), paths)
	}
	for _, st := range []struct{ uid, realm string }{{"g1", "global"}, {"c1", "cn"}} {
		status, _ := p.Status(st.uid)
		if status.Credits != 80 || len(status.CreditExpiries) != 1 || status.CreditExpiries[0].Remaining != 80 {
			t.Fatalf("%s (%s) expiry snapshot missing: %+v", st.uid, st.realm, status)
		}
	}
	for _, path := range paths {
		if strings.Contains(path, "checkin") {
			t.Fatalf("check-in endpoint must not be called, got %s", path)
		}
	}
}
