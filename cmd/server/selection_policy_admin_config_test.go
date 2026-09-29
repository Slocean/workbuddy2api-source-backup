package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 选号端点独立开关 + 分权启动校验（未通过验收修复交接 §4.1/§4.5）：
//   - selection_policy_enabled=true 时 management_key 必须非空且 ≠ api_key，
//     违反任一条启动失败（fail-fast，报错只说字段不回显密钥）；
//   - 未启用时空 management_key 合法（路由不注册，404）；
//   - env WB2A_SELECTION_POLICY_ADMIN_ENABLED 与 config 双入口同语义。

func TestSelectionPolicyEnabledRequiresManagementKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"admin":{"selection_policy_enabled":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("enabled without management key: start should fail")
	}
	if !strings.Contains(err.Error(), "management_key") {
		t.Fatalf("error should name the field, got: %v", err)
	}
	// env 开关同样触发该校验（config 未启用、env 启用）
	path2 := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path2, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WB2A_SELECTION_POLICY_ADMIN_ENABLED", "true")
	if _, err := Load(path2); err == nil {
		t.Fatal("env-enabled without management key: start should fail")
	}
}

func TestSelectionPolicyEnabledRejectsSameKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"admin":{"selection_policy_enabled":true,"management_key":"same"},"api_key":"same"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("same-value keys: start should fail")
	}
	// 报错不得回显密钥值
	if strings.Contains(err.Error(), "same") {
		t.Fatalf("error must not echo key values, got: %v", err)
	}
	// env 覆盖后同值同样拦截（config 合法、env 把两边配成同值）
	path2 := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path2, []byte(`{"admin":{"selection_policy_enabled":true,"management_key":"cfg-mgmt"},"api_key":"cfg-call"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WB2A_ADMIN_MANAGEMENT_KEY", "env-call")
	t.Setenv("WB2A_API_KEY", "env-call")
	if _, err := Load(path2); err == nil {
		t.Fatal("env same-value keys: start should fail")
	}
}

func TestSelectionPolicyDisabledEmptyKeyOK(t *testing.T) {
	// 未启用：management_key 空合法（现状部署形态——admin.enabled=false、无管理 Key）
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"admin":{"enabled":false},"api_key":"call-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("disabled endpoint with empty key should load: %v", err)
	}
	if cfg.Admin.SelectionPolicyEnabled {
		t.Fatal("selection_policy_enabled should default false")
	}
	// 启用且两把 Key 不同：合法
	path2 := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path2, []byte(`{"admin":{"selection_policy_enabled":true,"management_key":"mgmt"},"api_key":"call"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path2); err != nil {
		t.Fatalf("distinct keys should load: %v", err)
	}
}
