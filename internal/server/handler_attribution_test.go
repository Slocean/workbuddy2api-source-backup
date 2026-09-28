// handler_attribution_test.go 逐请求账号归属响应头（复核清单 §4.1 的 Go 侧契约）。
//
// 锁住三条性质：
//  1. 成功响应（流式与非流式）携带 X-ProxyHub-Account-UID =
//     本次最终实际承接的账号 + Source: actual；
//  2. 失败/无账号响应（503）**不携带**归属头——无真实成功承接时不伪造 UID；
//  3. 失败转移后（首选失败换号成功），头里的 UID 是**最终成功**的账号。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

const sseBadThenOK = "data: {\"error\":{\"code\":402,\"msg\":\"insufficient credit\"}}\n\n"

func assertAttribution(t *testing.T, rec *httptest.ResponseRecorder, wantUID string) {
	t.Helper()
	if got := rec.Header().Get("X-ProxyHub-Account-UID"); got != wantUID {
		t.Fatalf("X-ProxyHub-Account-UID=%q want %q", got, wantUID)
	}
	if got := rec.Header().Get("X-ProxyHub-Account-Source"); got != "actual" {
		t.Fatalf("X-ProxyHub-Account-Source=%q want actual", got)
	}
}

func TestAttributionHeaderNonStreamSuccess(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	assertAttribution(t, rec, "u1")
}

func TestAttributionHeaderStreamSuccess(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u-stream", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	// 流式：头必须随响应头一起可见（HTTP 头在首帧前写定，之后不再变化）
	assertAttribution(t, rec, "u-stream")
}

func TestAttributionHeaderFailoverUsesFinalAccount(t *testing.T) {
	// 首选号 hard credit 失败 → 轮换到第二号成功：头必须是**最终成功**的号，
	// 不是首选候选（与 Hub 侧"取最终响应"的口径闭环）。
	bad := &auth.Auth{UID: "u-bad", AccessToken: "at-bad", ExpiresAt: 9999999999}
	good := &auth.Auth{UID: "u-good", AccessToken: "at-good", ExpiresAt: 9999999999}
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		if authz == "Bearer at-bad" {
			return 402, `{"code":6004,"msg":"insufficient credit","selection_policy":"n/a"}`, false
		}
		return 200, sseOK, true
	})
	// 确定性：randInt64N 恒 0 → 先选 credits 相同的首个（u-bad）
	p := pool.New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(bad)
	p.Add(good)
	p.SetCredits(bad.UID, 1000)
	p.SetCredits(good.UID, 1000)
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls < 2 {
		t.Fatalf("expected rotation, calls=%d", calls)
	}
	assertAttribution(t, rec, "u-good")
}

func TestAttributionHeaderAbsentOnNoAccount(t *testing.T) {
	// 全池不可用（503）：没有真实成功承接，绝不伪造 UID（Hub 落 unknown 口径）。
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		t.Fatal("upstream should not be called")
		return 500, "", false
	})
	h := NewHandler(Config{
		Pool:     pool.New(""),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d", rec.Code)
	}
	if got := rec.Header().Get("X-ProxyHub-Account-UID"); got != "" {
		t.Fatalf("503 carried attribution header: %q", got)
	}
	if got := rec.Header().Get("X-ProxyHub-Account-Source"); got != "" {
		t.Fatalf("503 carried source header: %q", got)
	}
}
