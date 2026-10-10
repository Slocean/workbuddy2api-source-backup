package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

func TestScopePersistenceAndIndependentUpdates(t *testing.T) {
	h, path := newSelectionPolicyHandler(t, "mgmt", "call")
	rec := doPolicy(t, h, http.MethodPut, "mgmt", "{\"selection_scope\":\"all\"}")
	if rec.Code != 200 || h.cfg.Pool.SelectionScope() != "all" || h.cfg.Pool.SelectionPolicy() != "expiry_then_cheapest" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if LoadPersistedSelectionScope(path) != "all" || LoadPersistedSelectionPolicy(path) != "expiry_then_cheapest" {
		t.Fatal("restart differs")
	}
	rec = doPolicy(t, h, http.MethodPut, "mgmt", "{\"selection_policy\":\"cheapest\"}")
	if rec.Code != 200 || LoadPersistedSelectionScope(path) != "all" {
		t.Fatal("policy erased scope")
	}
	h.cfg.SelectionPolicyLocked = true
	rec = doPolicy(t, h, http.MethodPut, "mgmt", "{\"selection_scope\":\"prefer_cn\"}")
	if rec.Code != 200 {
		t.Fatal("policy lock blocked scope")
	}
	h.cfg.SelectionScopeLocked = true
	if rec = doPolicy(t, h, http.MethodPut, "mgmt", "{\"selection_scope\":\"all\"}"); rec.Code != 409 {
		t.Fatal("scope lock ignored")
	}
	h.cfg.SelectionPolicyLocked = false
	if rec = doPolicy(t, h, http.MethodPut, "mgmt", "{\"selection_policy\":\"balanced\"}"); rec.Code != 200 {
		t.Fatal("scope lock blocked policy")
	}
	for _, body := range []string{"{}", "{\"selection_scope\":\"\"}", "{\"selection_scope\":\"bad\"}"} {
		if rec = doPolicy(t, h, http.MethodPut, "mgmt", body); rec.Code != 400 {
			t.Fatal("invalid accepted", body)
		}
	}
	if err := persistSelectionPolicy(path, "cheapest"); err != nil {
		t.Fatal(err)
	}
	if LoadPersistedSelectionScope(path) != "" {
		t.Fatal("legacy file not preserved")
	}
}
// TestCatalogAllowsEvidenceSemantics 每账号目录证据判定语义（2026-10-10 反例修复）：
// 目录正命中=正证据（能力旗标把关）；完整目录缺失=权威否定；降级/未知缺失不构成
// 支持也不构成否定——同域保留 legacy 文本语义，跨域仅同账号新鲜实际成功观测放行
// 文本；图片/工具能力无目录证据不编造。
func TestCatalogAllowsEvidenceSemantics(t *testing.T) {
	fullWith := catalogEvidence{known: true, complete: true, infos: []upstream.ModelInfo{
		{ID: "m1", SupportsImages: true, SupportsToolCall: true},
		{ID: "text-only"},
	}}
	partial := catalogEvidence{known: true, complete: false, names: []string{"other"},
		infos: []upstream.ModelInfo{{ID: "m1", SupportsImages: true}}}
	unknown := catalogEvidence{}
	fresh := catalogEvidence{freshSuccess: true}

	// 完整目录正命中：能力旗标把关。
	if !catalogAllows(fullWith, "m1", true, true, true) {
		t.Fatal("complete catalog hit with capabilities must pass")
	}
	if catalogAllows(fullWith, "text-only", true, false, true) {
		t.Fatal("rich entry without image support must gate image requests")
	}
	// 完整目录缺失：权威否定——跨域/同域、即便有历史成功观测也一票排除。
	if catalogAllows(fullWith, "missing", false, false, true) {
		t.Fatal("complete catalog missing = authoritative negative (cross realm)")
	}
	if catalogAllows(fullWith, "missing", false, false, false) {
		t.Fatal("complete catalog missing = authoritative negative (same realm)")
	}
	completeFresh := fullWith
	completeFresh.freshSuccess = true
	if catalogAllows(completeFresh, "missing", false, false, true) {
		t.Fatal("fresh success must not override still-fresh complete catalog")
	}
	// 降级目录：命中按能力放行；缺失不构成支持——跨域不放行（除非新鲜成功观测）。
	if !catalogAllows(partial, "m1", true, false, true) {
		t.Fatal("partial catalog hit with image support must pass cross realm")
	}
	if catalogAllows(partial, "missing", false, false, true) {
		t.Fatal("partial catalog missing must NOT authorize cross realm")
	}
	if !catalogAllows(partial, "missing", false, false, false) {
		t.Fatal("same-realm legacy text semantics preserved for partial missing")
	}
	partialFresh := partial
	partialFresh.freshSuccess = true
	if !catalogAllows(partialFresh, "missing", false, false, true) {
		t.Fatal("fresh per-account success closes the partial gap (text)")
	}
	if catalogAllows(partialFresh, "missing", true, false, true) {
		t.Fatal("image capability without catalog evidence must not be fabricated")
	}
	// 未知目录：同域 legacy 文本保留；跨域不放行；有新鲜成功观测才放行文本。
	if !catalogAllows(unknown, "m1", false, false, false) {
		t.Fatal("unknown catalog same-realm legacy text preserved")
	}
	if catalogAllows(unknown, "m1", false, false, true) {
		t.Fatal("unknown catalog must NOT authorize cross realm")
	}
	if !catalogAllows(fresh, "m1", false, false, true) {
		t.Fatal("fresh per-account success is valid cross-realm positive evidence")
	}
	if catalogAllows(unknown, "m1", true, false, false) {
		t.Fatal("image capability must not be fabricated from unknown catalog")
	}
}

func TestScopeChatUsesSelectedAccountEndpointAndRealm(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	for _, stream := range []bool{false, true} {
		for _, scope := range []string{"prefer_global", "prefer_cn", "all"} {
			t.Run(scope+map[bool]string{true: "/stream", false: "/json"}[stream], func(t *testing.T) {
				resetModelsCache()
				t.Cleanup(resetModelsCache)
				p := pool.New("")
				p.Add(&auth.Auth{UID: "cn", Domain: "www.codebuddy.cn", AccessToken: "cn-token", ExpiresAt: 9999999999})
				p.Add(&auth.Auth{UID: "global", Domain: "www.workbuddy.ai", AccessToken: "global-token", ExpiresAt: 9999999999})
				p.SetSelectionConfig("cheapest", scope)
				p.NoteModelCost("cn", "glm-5.2", 1, 1000)
				p.NoteModelCost("global", "glm-5.2", 10, 1000)
				gotHost, gotAuth, gotModel := "", "", ""
				up := &upstream.Client{GlobalEnabled: true, ChatBaseCN: "https://cn.fake", ChatBaseGlobal: "https://global.fake",
					BillingBaseCN: "https://cn.fake", BillingBaseGlobal: "https://global.fake",
					HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
						content := "{\"code\":0,\"data\":{\"models\":[{\"id\":\"glm-5.2\",\"supportsToolCall\":true}]}}"
						ct := "application/json"
						if req.Method == http.MethodPost && strings.Contains(req.URL.Path, "chat") {
							gotHost, gotAuth = req.URL.Host, req.Header.Get("Authorization")
							var body map[string]any
							_ = json.NewDecoder(req.Body).Decode(&body)
							gotModel, _ = body["model"].(string)
							content, ct = sseOK, "text/event-stream"
						}
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{ct}}, Body: io.NopCloser(strings.NewReader(content))}, nil
					})}}
				h := NewHandler(Config{Pool: p, Upstream: up})
				body := "{\"model\":\"global:glm-5.2\",\"stream\":" + map[bool]string{true: "true", false: "false"}[stream] + ",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"
				req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				want := "cn"
				if scope == "prefer_global" {
					want = "global"
				}
				if rec.Code != 200 || gotHost != want+".fake" || gotAuth != "Bearer "+want+"-token" || gotModel != "glm-5.2" || rec.Header().Get("X-ProxyHub-Account-Realm") != want {
					t.Fatalf("code=%d host=%s auth=%s model=%s realm=%s body=%s", rec.Code, gotHost, gotAuth, gotModel, rec.Header().Get("X-ProxyHub-Account-Realm"), rec.Body)
				}
			})
		}
	}
}
