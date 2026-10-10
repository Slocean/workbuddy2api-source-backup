// catalog_evidence_http_test.go —— 每账号目录证据的端到端回归（真实 HTTP 全链路，假上游）。
//
// 场景（2026-10-10 反例）：v3/config 瞬时超时 → 企业端点降级窄表（不含 DeepSeek）
// 被当完整目录缓存 1h → scopeCompatibility 把真实支持 DeepSeek 的最早 global 账号
// 整域排除。修复语义：
//   - 目录证据按账号 UID 隔离（A 不冒充 B）；
//   - 完整目录缺失=权威否定；降级/未知缺失不构成支持也不构成否定；
//   - 跨域候选仅凭同账号新鲜实际成功观测（每账号成本台账）放行文本；
//   - 未知 global 账号在裸请求（requested=cn）下不得被选，主目录恢复后才选；
//   - GlobalEnabled=false 逃生门：global 域账号经 Auth.Realm() 回落 cn，与既有语义对齐。
package server

import (
	"fmt"
	"io"
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

// catalogFake 按账号（authz）与路径（v3 / 企业端点）供给目录的假上游：
// globalV3OK 控制主路径成败；企业端点恒返回不含 deepseek 的窄表；CN 目录完整含 deepseek。
type catalogFake struct {
	mu         sync.Mutex
	globalV3OK bool
	authz      string
	model      string
}

const (
	cnCatalogFull    = `{"code":0,"data":{"models":[{"id":"deepseek-v4.1-flash","supportsImages":true,"supportsToolCall":true}],"agents":[{"name":"cli","models":["deepseek-v4.1-flash"]}]}}`
	globalFull       = `{"code":0,"data":{"models":[{"id":"deepseek-v4.1-flash","supportsImages":true,"supportsToolCall":true}]}}`
	globalNarrow18   = `{"code":0,"data":{"models":[{"id":"gpt-5.6-sol"},{"id":"glm-5.2"},{"id":"kimi-k3"}]}}`
	globalV3FailBody = `{"code":500,"msg":"internal"}`
)

func (f *catalogFake) client() *upstream.Client {
	return &upstream.Client{
		GlobalEnabled: true,
		ChatBaseCN:    "https://cn.fake", ChatBaseGlobal: "https://global.fake",
		BillingBaseCN: "https://cn.fake", BillingBaseGlobal: "https://global.fake",
		HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodPost {
				f.mu.Lock()
				f.authz = req.Header.Get("Authorization")
				f.model = req.URL.Host // 域即账号身份（cn.fake=cn1 / global.fake=g1）
				f.mu.Unlock()
				return &http.Response{StatusCode: 200,
					Header: http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:   io.NopCloser(strings.NewReader(sseOK))}, nil
			}
			ct := "application/json"
			var body string
			switch {
			case req.URL.Host == "cn.fake":
				body = cnCatalogFull
			case strings.Contains(req.URL.Path, "/v3/config"):
				f.mu.Lock()
				ok := f.globalV3OK
				f.mu.Unlock()
				if !ok {
					body = globalV3FailBody
					return &http.Response{StatusCode: 500, Header: http.Header{"Content-Type": []string{ct}},
						Body: io.NopCloser(strings.NewReader(body))}, nil
				}
				body = globalFull
			default:
				body = globalNarrow18
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{ct}},
				Body: io.NopCloser(strings.NewReader(body))}, nil
		})},
	}
}

func (f *catalogFake) outbound() (authz, host string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authz, f.model
}

func (f *catalogFake) setGlobalV3OK(ok bool) {
	f.mu.Lock()
	f.globalV3OK = ok
	f.mu.Unlock()
}

// evidencePool 双账号池：g1（global）最早到期、cn1 次之。
func evidencePool() *pool.Pool {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn", AccessToken: "cn1-token", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "g1-token", ExpiresAt: 9999999999})
	p.SetCreditsWithExpiries("cn1", 1000, 0,
		[]pool.CreditExpiry{{Expiry: time.Now().Add(2 * 24 * time.Hour), Remaining: 1000}})
	p.SetCreditsWithExpiries("g1", 1000, 0,
		[]pool.CreditExpiry{{Expiry: time.Now().Add(24 * time.Hour), Remaining: 1000}})
	p.SetSelectionConfig("earliest_expiry", "all")
	return p
}

func doChat(t *testing.T, h *Handler, model string) *httptest.ResponseRecorder {
	t.Helper()
	payload := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(payload)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	return rec
}

// TestScopeUnknownGlobalNotSelectedBareCN 主目录失败（global 目录未知）时，跨域候选
// 不得凭「未知」放行：最早到期的是 g1 也不能选它——裸请求（requested=cn）选 cn1；
// 主目录真实恢复（完整目录含 DeepSeek）后才选 g1。另有新鲜实际成功观测的账号
// 立即凭台账放行（不等目录恢复）。
func TestScopeUnknownGlobalNotSelectedBareCN(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()
	t.Cleanup(resetModelsCache)

	f := &catalogFake{globalV3OK: false} // v3 失败 → 企业窄表降级（不含 deepseek）
	p := evidencePool()
	h := NewHandler(Config{Pool: p, Upstream: f.client()})

	// 1) g1 目录未知 + 无成功观测：跨域不放行 → 选 cn1（尽管 g1 更早到期）。
	rec := doChat(t, h, "deepseek-v4.1-flash")
	if authz, host := f.outbound(); authz != "Bearer cn1-token" || host != "cn.fake" {
		t.Fatalf("unknown global must NOT be selected cross realm: authz=%q host=%q", authz, host)
	}
	if realm := rec.Header().Get("X-ProxyHub-Account-Realm"); realm != "cn" {
		t.Fatalf("realm=%q want cn", realm)
	}

	// 2) g1 有新鲜实际成功观测（每账号台账，66 次历史的形态）：立即凭台账放行 → 选 g1。
	p.NoteModelCost("g1", "deepseek-v4.1-flash", 0, 100)
	rec = doChat(t, h, "deepseek-v4.1-flash")
	if authz, host := f.outbound(); authz != "Bearer g1-token" || host != "global.fake" {
		t.Fatalf("fresh per-account success must authorize cross realm: authz=%q host=%q", authz, host)
	}
	if realm := rec.Header().Get("X-ProxyHub-Account-Realm"); realm != "global" {
		t.Fatalf("realm=%q want global", realm)
	}

	// 3) 主目录恢复（完整目录含 DeepSeek，新缓存）+ 台账清空：仍选 g1（目录正证据）。
	p2 := evidencePool()
	f2 := &catalogFake{globalV3OK: true}
	h2 := NewHandler(Config{Pool: p2, Upstream: f2.client()})
	rec = doChat(t, h2, "deepseek-v4.1-flash")
	if authz, host := f2.outbound(); authz != "Bearer g1-token" || host != "global.fake" {
		t.Fatalf("recovered main catalog must authorize cross realm: authz=%q host=%q", authz, host)
	}
	if realm := rec.Header().Get("X-ProxyHub-Account-Realm"); realm != "global" {
		t.Fatalf("realm=%q want global", realm)
	}
}

// TestScopeCompleteCatalogMissingIsAuthoritative 完整目录缺失是权威否定：
// 即便账号有历史成功观测，也不得跨域选中（成功观测不推翻仍新鲜的完整目录）。
func TestScopeCompleteCatalogMissingIsAuthoritative(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()
	t.Cleanup(resetModelsCache)

	// v3 与企业端点都成功，但目录不含 deepseek（窄表形态被企业端点真实下发）。
	f := &catalogFake{globalV3OK: true}
	f.mu.Lock()
	f.globalV3OK = true
	f.mu.Unlock()
	// 把 global 两路目录都换成「无 deepseek 的完整窄表」：v3 与企业同答。
	up := f.client()
	up.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost {
			f.mu.Lock()
			f.authz = req.Header.Get("Authorization")
			f.model = req.URL.Host
			f.mu.Unlock()
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(sseOK))}, nil
		}
		ct := "application/json"
		var body string
		switch {
		case req.URL.Host == "cn.fake":
			body = cnCatalogFull
		default:
			body = globalNarrow18 // global 完整目录（两路一致）不含 deepseek
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{ct}},
			Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	p := evidencePool()
	p.NoteModelCost("g1", "deepseek-v4.1-flash", 0, 100) // 历史成功观测在场
	h := NewHandler(Config{Pool: p, Upstream: up})
	doChat(t, h, "deepseek-v4.1-flash")
	if authz, host := f.outbound(); authz != "Bearer cn1-token" || host != "cn.fake" {
		t.Fatalf("complete catalog missing is authoritative despite fresh success: authz=%q host=%q", authz, host)
	}
}

// TestScopeGlobalDisabledEscapeHatchChat GlobalEnabled=false 逃生门与既有语义对齐：
// Auth.Realm() 把 global 域账号回落 cn（auth 层锁死），账号以 CN 证据路径参与选号，
// 归属头如实回落 cn；不 panic、不放行为「无证据跨域」。
func TestScopeGlobalDisabledEscapeHatchChat(t *testing.T) {
	auth.SetGlobalEnabled(false)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()
	t.Cleanup(resetModelsCache)

	f := &catalogFake{globalV3OK: true}
	p := evidencePool()
	h := NewHandler(Config{Pool: p, Upstream: f.client()})
	rec := doChat(t, h, "deepseek-v4.1-flash")
	// edsionss(g1) 最早到期，但逃生门下 Realm() 回落 cn——其目录按 CN 路径探测
	// （fake 对 cn.fake 返回含 deepseek 的 CN 目录），归属域如实为 cn。
	if authz, _ := f.outbound(); authz != "Bearer g1-token" && authz != "Bearer cn1-token" {
		t.Fatalf("unexpected account: authz=%q", authz)
	}
	if realm := rec.Header().Get("X-ProxyHub-Account-Realm"); realm != "cn" {
		t.Fatalf("escape hatch: realm must fall back to cn, got %q", realm)
	}
}
