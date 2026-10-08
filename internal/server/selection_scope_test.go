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
func TestScopeCatalogCompatibility(t *testing.T) {
	cn := &auth.Auth{Domain: "www.codebuddy.cn"}
	global := &auth.Auth{Domain: "www.workbuddy.ai"}
	infos := []upstream.ModelInfo{{ID: "shared", SupportsImages: true, SupportsToolCall: true}, {ID: "text"}}
	allowed := catalogCompatibility("shared", "global", true, true, infos, []string{"shared"}, infos)
	if !allowed(cn) || !allowed(global) {
		t.Fatal("shared compatible model excluded")
	}
	allowed = catalogCompatibility("missing", "global", false, false, infos, []string{"missing"}, nil)
	if allowed(cn) || !allowed(global) {
		t.Fatal("model absence not filtered")
	}
	allowed = catalogCompatibility("shared", "global", false, false, nil, nil, nil)
	if allowed(cn) || !allowed(global) {
		t.Fatal("unknown catalog authorized crossing")
	}
	allowed = catalogCompatibility("shared", "global", true, false, nil, []string{"shared"}, nil)
	if allowed(global) {
		t.Fatal("name-only catalog fabricated image support")
	}
	allowed = catalogCompatibility("text", "cn", true, false, infos, nil, nil)
	if allowed(cn) {
		t.Fatal("image incompatible selected")
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
