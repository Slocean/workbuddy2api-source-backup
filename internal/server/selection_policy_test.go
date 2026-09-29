// selection_policy_test.go 选号策略管理端点（复核清单 §3/§4.2 的 Go 侧契约）。
//
// 锁住四条性质：
//  1. 凭证契约：/admin/pool/selection-policy 只认**独立管理 Key**，
//     普通模型调用 Key（api_key）即使合法也 401，且不产生任何状态变化；
//  2. GET 返回 Hub 依赖的完整形状（supported/current/effective/source/
//     restart_required/version），current 与 effective 语义分明；
//  3. PUT 校验白名单；被部署配置（env）锁定时 409 且不改现状；
//  4. PUT 成功 = 运行时立即生效 + data 文件持久化，重启读回一致
//     （LoadPersistedSelectionPolicy 幂等），损坏文件安全回落空值。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/pool"
)

func newSelectionPolicyHandler(t *testing.T, mgmtKey, apiKey string) (*Handler, string) {
	t.Helper()
	dataPath := filepath.Join(t.TempDir(), "selection-policy.json")
	p := pool.New("")
	// 生产装配（cmd/server/main.go）会用 config 初始值注入池；测试同型：
	// config.json 默认 expiry_then_cheapest。
	p.SetSelectionPolicy("expiry_then_cheapest")
	h := NewHandler(Config{
		Pool: p,
		// 选号端点独立开关（未通过验收修复交接 §4.2）：与 AdminEnabled 拆开，
		// 本 helper 只启用选号面——账号状态写端点的注册与此无关。
		AdminSelectionPolicyEnabled: true,
		AdminManagementKey:          mgmtKey,
		APIKey:                      apiKey,
		SelectionPolicyConfigValue:  "expiry_then_cheapest",
		SelectionPolicyDataPath:     dataPath,
	})
	return h, dataPath
}

func doPolicy(t *testing.T, h *Handler, method, key string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, "/admin/pool/selection-policy", nil)
	} else {
		req = httptest.NewRequest(method, "/admin/pool/selection-policy", strings.NewReader(body))
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSelectionPolicyCallKeyRejected(t *testing.T) {
	// 普通模型调用 Key 不是管理凭证：401，且策略保持不变（修复清单 §3.1 契约，
	// 本服务强制执行，不依赖 Hub 侧自律）。
	h, _ := newSelectionPolicyHandler(t, "mgmt-secret", "call-key")
	rec := doPolicy(t, h, http.MethodGet, "call-key", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("call key GET code=%d want 401", rec.Code)
	}
	rec = doPolicy(t, h, http.MethodPut, "call-key", `{"selection_policy":"cheapest"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("call key PUT code=%d want 401", rec.Code)
	}
	if got := h.cfg.Pool.SelectionPolicy(); got != "expiry_then_cheapest" {
		t.Fatalf("policy changed by call key: %s", got)
	}
}

func TestSelectionPolicyNoManagementKeyAllDenied(t *testing.T) {
	// 管理凭证未配置：对任何凭证都 401（不存在"未配置即放行"的退化路径）。
	h, _ := newSelectionPolicyHandler(t, "", "call-key")
	rec := doPolicy(t, h, http.MethodGet, "call-key", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("empty mgmt key GET code=%d want 401", rec.Code)
	}
	rec = doPolicy(t, h, http.MethodGet, "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET code=%d want 401", rec.Code)
	}
}

func TestSelectionPolicyGetShape(t *testing.T) {
	h, _ := newSelectionPolicyHandler(t, "mgmt-secret", "call-key")
	rec := doPolicy(t, h, http.MethodGet, "mgmt-secret", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET code=%d body=%s", rec.Code, rec.Body)
	}
	var snap selectionPolicySnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("not json: %v", err)
	}
	if !snap.Supported {
		t.Errorf("supported=false")
	}
	// 无 data 文件时 current 来自 config 初始值
	if snap.Current != "expiry_then_cheapest" || snap.Source != "config.json" {
		t.Errorf("current=%q source=%q", snap.Current, snap.Source)
	}
	if snap.Effective != "expiry_then_cheapest" {
		t.Errorf("effective=%q", snap.Effective)
	}
	if snap.RestartRequired {
		t.Errorf("restart_required should be false for runtime-apply build")
	}
	if !strings.Contains(snap.Version, ServiceName) {
		t.Errorf("version=%q", snap.Version)
	}
}

func TestSelectionPolicyPutValidatesAndPersists(t *testing.T) {
	h, dataPath := newSelectionPolicyHandler(t, "mgmt-secret", "call-key")
	// 非法值 400，不落盘
	rec := doPolicy(t, h, http.MethodPut, "mgmt-secret", `{"selection_policy":"hack"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid policy code=%d", rec.Code)
	}
	if _, err := os.Stat(dataPath); !os.IsNotExist(err) {
		t.Fatalf("invalid PUT created data file")
	}
	// 合法值：立即生效 + 持久化
	rec = doPolicy(t, h, http.MethodPut, "mgmt-secret", `{"selection_policy":"cheapest"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT code=%d body=%s", rec.Code, rec.Body)
	}
	var snap selectionPolicySnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("not json: %v", err)
	}
	if snap.Effective != "cheapest" || snap.Current != "cheapest" {
		t.Fatalf("after PUT effective=%s current=%s", snap.Effective, snap.Current)
	}
	if snap.Source != "data/selection-policy.json" {
		t.Errorf("source after PUT=%q", snap.Source)
	}
	// 重启读回：LoadPersistedSelectionPolicy 与 GET 的 current 一致
	if v := LoadPersistedSelectionPolicy(dataPath); v != "cheapest" {
		t.Fatalf("persisted=%q want cheapest", v)
	}
	// 再 GET：current 来自 data 文件
	rec = doPolicy(t, h, http.MethodGet, "mgmt-secret", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("not json: %v", err)
	}
	if snap.Current != "cheapest" || snap.Effective != "cheapest" {
		t.Errorf("after restart-read current=%s effective=%s", snap.Current, snap.Effective)
	}
}

func TestSelectionPolicyPutLockedByEnv(t *testing.T) {
	h, dataPath := newSelectionPolicyHandler(t, "mgmt-secret", "call-key")
	h.cfg.SelectionPolicyLocked = true
	h.cfg.SelectionPolicyEnvValue = "cheapest"
	rec := doPolicy(t, h, http.MethodPut, "mgmt-secret", `{"selection_policy":"balanced"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("locked PUT code=%d want 409 body=%s", rec.Code, rec.Body)
	}
	if got := h.cfg.Pool.SelectionPolicy(); got != "expiry_then_cheapest" {
		t.Fatalf("locked PUT changed policy to %s", got)
	}
	if _, err := os.Stat(dataPath); !os.IsNotExist(err) {
		t.Fatalf("locked PUT wrote data file")
	}
}

func TestLoadPersistedSelectionPolicyCorruptFallsBackEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "selection-policy.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v := LoadPersistedSelectionPolicy(path); v != "" {
		t.Fatalf("corrupt file returned %q", v)
	}
	// 白名单外的值同样拒绝
	if err := os.WriteFile(path, []byte(`{"selection_policy":"hack"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if v := LoadPersistedSelectionPolicy(path); v != "" {
		t.Fatalf("non-whitelist value returned %q", v)
	}
}

// TestSelectionPolicyEndpointDisabled404 开关拆分（未通过验收修复交接 §4.2/§4.5）：
// 选号端点未启用时路由不注册，GET/PUT 一律 404——即使 admin.enabled=true（账号
// 写面开着）也不注册选号面；对未鉴权探测与真 404 不可区分。
func TestSelectionPolicyEndpointDisabled404(t *testing.T) {
	p := pool.New("")
	p.SetSelectionPolicy("expiry_then_cheapest")
	h := NewHandler(Config{
		Pool:                       p,
		AdminEnabled:               true, // 账号写面开着：选号面仍不得出现
		AdminSelectionPolicyEnabled: false,
		AdminManagementKey:         "mgmt-secret",
		APIKey:                     "call-key",
		SelectionPolicyConfigValue: "expiry_then_cheapest",
	})
	for _, tc := range []struct {
		method, key, body string
	}{
		{http.MethodGet, "", ""},
		{http.MethodGet, "mgmt-secret", ""},
		{http.MethodGet, "call-key", ""},
		{http.MethodPut, "", `{"selection_policy":"cheapest"}`},
		{http.MethodPut, "mgmt-secret", `{"selection_policy":"cheapest"}`},
		{http.MethodPut, "call-key", `{"selection_policy":"cheapest"}`},
	} {
		rec := doPolicy(t, h, tc.method, tc.key, tc.body)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s key=%q code=%d want 404", tc.method, tc.key, rec.Code)
		}
	}
	if got := h.cfg.Pool.SelectionPolicy(); got != "expiry_then_cheapest" {
		t.Fatalf("disabled endpoint changed policy to %s", got)
	}
}

// TestSelectionPolicyIndependentOfAdminWriteEndpoints 只开选号开关（admin.enabled
// 关闭）时：选号 GET/PUT 用管理 Key 可用；三个账号状态修改端点不注册（404）——
// 启用选号管理不顺带开放账号写面（未通过验收修复交接 §4.3）。
func TestSelectionPolicyIndependentOfAdminWriteEndpoints(t *testing.T) {
	p := pool.New("")
	p.SetSelectionPolicy("expiry_then_cheapest")
	h := NewHandler(Config{
		Pool:                        p,
		AdminEnabled:                false,
		AdminSelectionPolicyEnabled: true,
		AdminManagementKey:          "mgmt-secret",
		APIKey:                      "call-key",
		SelectionPolicyConfigValue:  "expiry_then_cheapest",
		SelectionPolicyDataPath:     filepath.Join(t.TempDir(), "selection-policy.json"),
	})
	// 选号面可用（管理 Key 200）
	if rec := doPolicy(t, h, http.MethodGet, "mgmt-secret", ""); rec.Code != http.StatusOK {
		t.Fatalf("selection GET code=%d want 200", rec.Code)
	}
	// 账号状态写端点 404：无论匿名 / 调用 Key / 管理 Key，路由都未注册
	for _, key := range []string{"", "call-key", "mgmt-secret"} {
		req := httptest.NewRequest(http.MethodPost, "/admin/accounts/uid-x/disable", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("admin disable key=%q code=%d want 404", key, rec.Code)
		}
	}
}

// TestSelectionPolicySameValueKeysRejectedAtHandler 防御性同值检查（§4.5）：绕过
// cmd/server 启动校验、直接构造 Config 把两把 Key 配成同值时，鉴权层必须独立拒绝
// ——同值即无分权，管理面不得放行（即使请求带的就是那把"管理 Key"）。
func TestSelectionPolicySameValueKeysRejectedAtHandler(t *testing.T) {
	h, _ := newSelectionPolicyHandler(t, "same-key", "same-key")
	for _, key := range []string{"same-key", "", "other"} {
		rec := doPolicy(t, h, http.MethodGet, key, "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("same-value keys GET key=%q code=%d want 401", key, rec.Code)
		}
	}
}
