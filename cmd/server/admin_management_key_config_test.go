package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 独立管理凭证（复核清单 §3.2）：config 与 env 两条入口，且与调用 Key 严格分离。
func TestAdminManagementKeyConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"admin":{"enabled":true,"management_key":"cfg-key"},"api_key":"call-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Admin.ManagementKey != "cfg-key" {
		t.Fatalf("management key = %q", cfg.Admin.ManagementKey)
	}
	if cfg.APIKey != "call-key" {
		t.Fatalf("api key = %q", cfg.APIKey)
	}
	// env 覆盖 config（与 WB2A_SELECTION_POLICY 同一优先级纪律）
	t.Setenv("WB2A_ADMIN_MANAGEMENT_KEY", "env-key")
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Admin.ManagementKey != "env-key" {
		t.Fatalf("env override = %q", cfg.Admin.ManagementKey)
	}
}
