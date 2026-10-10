// selection_expiry_http_test.go ——「聚合版本 + 最早到期」选号回归（真实 HTTP 全链路，假上游）。
//
// 背景（2026-10-10 用户问题）：deepseek-v4.1-flash 双域同模型、国际号积分更早到期时，
// selection_scope=all + earliest_expiry 必须选国际号；Hub 侧把裸模型映射成 cn: 前缀
// 送达 wb2api 也不得改变该结果（scope=all 下 cn: 映射必须与裸名等价）。同时钉住：
// 不支持该模型的更早到期号必须排除（不得换模型凑成功）、跨域同模型纯文本不因能力
// 字段差异被排除而图片/工具仍按域目录限制、X-ProxyHub-Allowed-Realm 硬权限边界、
// prefer_* 在兼容候选内生效、粘性号在更优到期号出现时重绑、未知/已过期积分后置。
//
// 边界：全部走 httptest 假上游（目录探测 + chat 出站捕获），不触真实账号；
// global_e2e build tag 的真账号实测不进本文件。
package server

import (
	"encoding/json"
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
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// expModel 双域同模型（生产实证：cn:/global:deepseek-v4.1-flash 同时在目录）。
const expModel = "deepseek-v4.1-flash"

// catalogJSON 构造 {code:0,data:{models:[…]}} 目录形态（与 dynModelEntry 解析对齐），
// images/tools 控制每域目录条目的能力旗标——两域字段允许不同（生产即仅 reasoning 档/倍率不同）。
func catalogJSON(images, tools bool, ids ...string) string {
	models := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		models = append(models, map[string]any{"id": id, "supportsImages": images, "supportsToolCall": tools})
	}
	// CN 企业路解析要求 agents[cli] 名单（缺失则该路失败 → 目录降级 incomplete），
	// 夹具统一带上 agents 使 CN 目录成为完整证据。
	raw, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{
		"models": models,
		"agents": []map[string]any{{"name": "cli", "models": ids}},
	}})
	return string(raw)
}

// realmCapture 捕获出站 chat 的账号与模型（轮转内单次成功即定论）。
type realmCapture struct {
	mu    sync.Mutex
	authz string
	model string
}

func (c *realmCapture) snapshot() (authz, model string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.authz, c.model
}

// newExpiryFake 双域假上游：GET 目录探测按 host 返回各域目录；POST chat 捕获并回 SSE。
func newExpiryFake(cnCatalog, glCatalog string) (*upstream.Client, *realmCapture) {
	cap := &realmCapture{}
	up := &upstream.Client{
		GlobalEnabled: true,
		ChatBaseCN:    "https://cn.fake", ChatBaseGlobal: "https://global.fake",
		BillingBaseCN: "https://cn.fake", BillingBaseGlobal: "https://global.fake",
		HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodPost {
				raw, _ := io.ReadAll(req.Body)
				var body map[string]any
				_ = json.Unmarshal(raw, &body)
				cap.mu.Lock()
				cap.authz = req.Header.Get("Authorization")
				cap.model, _ = body["model"].(string)
				cap.mu.Unlock()
				return &http.Response{StatusCode: 200,
					Header: http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:   io.NopCloser(strings.NewReader(sseOK))}, nil
			}
			body := cnCatalog
			if req.URL.Host == "global.fake" {
				body = glCatalog
			}
			return &http.Response{StatusCode: 200,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body:   io.NopCloser(strings.NewReader(body))}, nil
		})},
	}
	return up, cap
}

// expiryPool 双账号池；days<0 表示该账号不给积分观测（earliestCreditExpiry 零值→排序后置）。
func expiryPool(cnDays, glDays int) *pool.Pool {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn", AccessToken: "cn1-token", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "g1-token", ExpiresAt: 9999999999})
	if cnDays >= 0 {
		p.SetCreditsWithExpiries("cn1", 1000, 1000,
			[]pool.CreditExpiry{{Expiry: time.Now().Add(time.Duration(cnDays) * 24 * time.Hour), Remaining: 1000}})
	}
	if glDays >= 0 {
		p.SetCreditsWithExpiries("g1", 1000, 1000,
			[]pool.CreditExpiry{{Expiry: time.Now().Add(time.Duration(glDays) * 24 * time.Hour), Remaining: 1000}})
	}
	return p
}

// TestScopeAllEarliestExpiryAcrossRealms 核心回归：scope=all + earliest_expiry 按最早积分
// 到期跨域选号；裸名与 Hub 映射形态 cn: 同判（聚合范围忽略请求域）；出站恒为裸名；
// 流式/非流式归属头（UID + 实际域）如实。
func TestScopeAllEarliestExpiryAcrossRealms(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	cases := []struct {
		name      string
		cnDays    int
		glDays    int
		wantUID   string
		wantRealm string
	}{
		{"global_earlier", 10, 1, "g1", "global"},
		{"cn_earlier", 1, 10, "cn1", "cn"},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			for _, model := range []string{expModel, "cn:" + expModel} {
				name := fmt.Sprintf("%s/stream=%v/model=%s", tc.name, stream, model)
				t.Run(name, func(t *testing.T) {
					resetModelsCache()
					t.Cleanup(resetModelsCache)
					catalog := catalogJSON(true, true, expModel)
					up, cap := newExpiryFake(catalog, catalog)
					p := expiryPool(tc.cnDays, tc.glDays)
					p.SetSelectionConfig("earliest_expiry", "all")
					h := NewHandler(Config{Pool: p, Upstream: up})
					payload := fmt.Sprintf(`{"model":%q,"stream":%v,"messages":[{"role":"user","content":"hi"}]}`, model, stream)
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(payload)))
					if rec.Code != 200 {
						t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
					}
					gotAuthz, gotModel := cap.snapshot()
					if gotAuthz != "Bearer "+tc.wantUID+"-token" {
						t.Fatalf("outbound account authz=%q want %s (earliest expiry must win across realms)", gotAuthz, tc.wantUID)
					}
					if gotModel != expModel {
						t.Fatalf("outbound model=%q want bare %q", gotModel, expModel)
					}
					if uid := rec.Header().Get("X-ProxyHub-Account-UID"); uid != tc.wantUID {
						t.Fatalf("attribution uid=%q want %q", uid, tc.wantUID)
					}
					if realm := rec.Header().Get("X-ProxyHub-Account-Realm"); realm != tc.wantRealm {
						t.Fatalf("attribution realm=%q want %q", realm, tc.wantRealm)
					}
					if stream && !strings.Contains(rec.Body.String(), "chat.completion.chunk") {
						t.Fatalf("stream body missing chunk frame: %s", rec.Body.String())
					}
				})
			}
		}
	}
}

// TestScopeAllEarliestExpiryExcludesUnsupportedEarlier 更早到期但不支持该模型的账号必须
// 排除（目录无此模型 / 11102 模型级冷却），不得换模型凑成功；出站模型保持请求原样。
func TestScopeAllEarliestExpiryExcludesUnsupportedEarlier(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	for _, tc := range []struct {
		name       string
		glCatalog  string
		blockModel bool
	}{
		{"global_catalog_lacks_model", catalogJSON(true, true, "other-model"), false},
		{"global_model_cooldown", catalogJSON(true, true, expModel), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetModelsCache()
			t.Cleanup(resetModelsCache)
			up, cap := newExpiryFake(catalogJSON(true, true, expModel), tc.glCatalog)
			p := expiryPool(10, 1) // g1 更早到期
			p.SetSelectionConfig("earliest_expiry", "all")
			if tc.blockModel {
				p.BlockModelBackoff("g1", expModel, "11102 test")
			}
			h := NewHandler(Config{Pool: p, Upstream: up})
			payload := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, expModel)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(payload)))
			if rec.Code != 200 {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
			}
			gotAuthz, gotModel := cap.snapshot()
			if gotAuthz != "Bearer cn1-token" {
				t.Fatalf("unsupported earlier account must be excluded, outbound authz=%q", gotAuthz)
			}
			if gotModel != expModel {
				t.Fatalf("model must not be substituted: %q", gotModel)
			}
			if realm := rec.Header().Get("X-ProxyHub-Account-Realm"); realm != "cn" {
				t.Fatalf("attribution realm=%q want cn", realm)
			}
		})
	}
}

// TestScopeAllCapabilityGatingPerRealm 跨域同模型：纯文本不因能力字段差异被排除
// （国际号更早即选国际）；带图/带工具仍按各域目录能力限制。
func TestScopeAllCapabilityGatingPerRealm(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	cnCatalog := catalogJSON(true, true, expModel)           // CN：图文工具全支持
	glCatalog := catalogJSON(false, false, expModel)         // 国际：仅文本（能力字段不同）
	for _, tc := range []struct {
		name     string
		messages string
		wantUID  string
	}{
		{"text_ignores_capability_diff", `[{"role":"user","content":"hi"}]`, "g1"},
		{"image_respects_catalog", `[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,a"}}]}]`, "cn1"},
		{"tools_respect_catalog", `[{"role":"user","content":"hi"}]`, "cn1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetModelsCache()
			t.Cleanup(resetModelsCache)
			up, cap := newExpiryFake(cnCatalog, glCatalog)
			p := expiryPool(10, 1) // g1 更早到期
			p.SetSelectionConfig("earliest_expiry", "all")
			h := NewHandler(Config{Pool: p, Upstream: up})
			payload := `{"model":"` + expModel + `","messages":` + tc.messages
			if tc.name == "tools_respect_catalog" {
				payload += `,"tools":[{"type":"function","function":{"name":"f","parameters":{}}}]`
			}
			payload += `}`
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(payload)))
			if rec.Code != 200 {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
			}
			gotAuthz, _ := cap.snapshot()
			if gotAuthz != "Bearer "+tc.wantUID+"-token" {
				t.Fatalf("%s: outbound authz=%q want %s", tc.name, gotAuthz, tc.wantUID)
			}
		})
	}
}

// TestHardRealmAllowedRealmGatesScopeAll X-ProxyHub-Allowed-Realm 是硬权限：
// scope=all 也必须被它收窄。Hub 侧该头只在 key 域限定或白名单收窄时发送
// （account_realm_constraint 先剥掉请求显式前缀再算硬域——显式前缀本身不产生
// 硬权限，只作为映射偏好）；本测试验证 Go 侧对在场头的执行语义。
func TestHardRealmAllowedRealmGatesScopeAll(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	for _, tc := range []struct{ hard, wantUID string }{
		{"cn", "cn1"}, // g1 更早到期也被硬权限排除
		{"global", "g1"},
	} {
		t.Run("realm="+tc.hard, func(t *testing.T) {
			resetModelsCache()
			t.Cleanup(resetModelsCache)
			catalog := catalogJSON(true, true, expModel)
			up, cap := newExpiryFake(catalog, catalog)
			p := expiryPool(10, 1)
			p.SetSelectionConfig("earliest_expiry", "all")
			h := NewHandler(Config{Pool: p, Upstream: up})
			payload := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, expModel)
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(payload))
			req.Header.Set("X-ProxyHub-Allowed-Realm", tc.hard)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
			}
			gotAuthz, _ := cap.snapshot()
			if gotAuthz != "Bearer "+tc.wantUID+"-token" {
				t.Fatalf("hard realm %s: authz=%q want %s", tc.hard, gotAuthz, tc.wantUID)
			}
		})
	}
}

// TestPreferScopeWithinCompatibleCandidates prefer_* 在模型兼容候选内优先相应池，
// 仍不排除他域兜底之外的策略语义（此处：池内有兼容号即按优先域选）。
func TestPreferScopeWithinCompatibleCandidates(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	for _, tc := range []struct{ scope, wantUID string }{
		{"prefer_global", "g1"}, // cn1 更早到期，但 prefer_global 在兼容候选内优先国际
		{"prefer_cn", "cn1"},
	} {
		t.Run("scope="+tc.scope, func(t *testing.T) {
			resetModelsCache()
			t.Cleanup(resetModelsCache)
			catalog := catalogJSON(true, true, expModel)
			up, cap := newExpiryFake(catalog, catalog)
			p := expiryPool(1, 10)
			p.SetSelectionConfig("earliest_expiry", tc.scope)
			h := NewHandler(Config{Pool: p, Upstream: up})
			payload := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, expModel)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(payload)))
			if rec.Code != 200 {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
			}
			gotAuthz, _ := cap.snapshot()
			if gotAuthz != "Bearer "+tc.wantUID+"-token" {
				t.Fatalf("scope=%s authz=%q want %s", tc.scope, gotAuthz, tc.wantUID)
			}
		})
	}
}

// TestStickyRebindsOnEarlierExpiry 粘性号不得抢占最早到期策略：更优到期号出现后，
// 下一请求必须解绑重选并收敛到新号（归属与绑定同步）。
func TestStickyRebindsOnEarlierExpiry(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()
	t.Cleanup(resetModelsCache)
	catalog := catalogJSON(true, true, expModel)
	up, cap := newExpiryFake(catalog, catalog)
	p := expiryPool(1, 10) // 初态 cn1 更早
	p.SetSelectionConfig("earliest_expiry", "all")
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"cn1", "g1"} },
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess})
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-exp"}}`, expModel)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("first code=%d body=%s", rec.Code, rec.Body.String())
	}
	gotAuthz, _ := cap.snapshot()
	if gotAuthz != "Bearer cn1-token" {
		t.Fatalf("first pick authz=%q want cn1 (cn1 earliest initially)", gotAuthz)
	}
	if uid, ok := st.lastUID("conv-exp"); !ok || uid != "cn1" {
		t.Fatalf("sticky binding=%v ok=%v want cn1", uid, ok)
	}

	// g1 变成更早到期 → 粘性必须让位（解绑重选）。
	p.SetCreditsWithExpiries("g1", 1000, 1000,
		[]pool.CreditExpiry{{Expiry: time.Now().Add(12 * time.Hour), Remaining: 1000}})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("second code=%d body=%s", rec.Code, rec.Body.String())
	}
	gotAuthz, _ = cap.snapshot()
	if gotAuthz != "Bearer g1-token" {
		t.Fatalf("sticky must yield to earlier expiry, authz=%q", gotAuthz)
	}
	if realm := rec.Header().Get("X-ProxyHub-Account-Realm"); realm != "global" {
		t.Fatalf("attribution realm=%q want global", realm)
	}
	if uid, ok := st.lastUID("conv-exp"); !ok || uid != "g1" {
		t.Fatalf("binding should rebind to g1, got %v ok=%v", uid, ok)
	}
}

// TestUnknownOrExpiredExpiryRanksLast 未知到期（无观测/观测过期）与已过期积分包
// 一律后置，不得抢先于有真实更早到期的账号。
func TestUnknownOrExpiredExpiryRanksLast(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	for _, tc := range []struct {
		name  string
		glSet func(*pool.Pool)
	}{
		{"no_observation", func(*pool.Pool) {}},
		{"expired_packages_only", func(p *pool.Pool) {
			p.SetCreditsWithExpiries("g1", 1000, 0,
				[]pool.CreditExpiry{{Expiry: time.Now().Add(-time.Hour), Remaining: 1000}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetModelsCache()
			t.Cleanup(resetModelsCache)
			catalog := catalogJSON(true, true, expModel)
			up, cap := newExpiryFake(catalog, catalog)
			p := expiryPool(10, -1) // cn1 已知 +10d；g1 按场景无有效到期
			tc.glSet(p)
			p.SetSelectionConfig("earliest_expiry", "all")
			h := NewHandler(Config{Pool: p, Upstream: up})
			payload := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, expModel)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(payload)))
			if rec.Code != 200 {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
			}
			gotAuthz, _ := cap.snapshot()
			if gotAuthz != "Bearer cn1-token" {
				t.Fatalf("%s: unknown/expired expiry must rank last, authz=%q", tc.name, gotAuthz)
			}
		})
	}
}

// TestGlobalOnlyBareModelCallableUnderAll global-only 裸目录模型在 scope=all 下可调
// （国际目录独有模型以裸名请求 → 选国际号；CN 目录无此模型被排除）。
func TestGlobalOnlyBareModelCallableUnderAll(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()
	t.Cleanup(resetModelsCache)
	up, cap := newExpiryFake(catalogJSON(true, true, "cn-model"), catalogJSON(true, true, "global-only-model"))
	p := expiryPool(1, 10) // cn1 更早到期，但目录不支持该模型
	p.SetSelectionConfig("earliest_expiry", "all")
	h := NewHandler(Config{Pool: p, Upstream: up})
	payload := `{"model":"global-only-model","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(payload)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	gotAuthz, gotModel := cap.snapshot()
	if gotAuthz != "Bearer g1-token" {
		t.Fatalf("global-only bare model must serve via g1, authz=%q", gotAuthz)
	}
	if gotModel != "global-only-model" {
		t.Fatalf("outbound model=%q", gotModel)
	}
}
