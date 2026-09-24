package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSelectionPolicyConfig(t *testing.T) {
	if got := Default().Pool.SelectionPolicy; got != "expiry_then_cheapest" {
		t.Fatalf("default policy = %q", got)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"pool":{"selection_policy":"cheapest_then_expiry"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pool.SelectionPolicy != "cheapest_then_expiry" {
		t.Fatalf("configured policy = %q, err=%v", cfg.Pool.SelectionPolicy, err)
	}
	if err := os.WriteFile(path, []byte(`{"pool":{"selection_policy":"unknown"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("invalid policy accepted")
	}
}
