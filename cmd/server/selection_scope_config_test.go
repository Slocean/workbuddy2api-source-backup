package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSelectionScopeConfigAndEnv(t *testing.T) {
	t.Setenv("WB2A_SELECTION_SCOPE", "")
	if got := Default().Pool.SelectionScope; got != "" {
		t.Fatal("legacy default changed")
	}
	path := filepath.Join(t.TempDir(), "config.json")
	for _, scope := range []string{"prefer_global", "prefer_cn", "all", "bad"} {
		if err := os.WriteFile(path, []byte("{\"pool\":{\"selection_scope\":\""+scope+"\"}}"), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if scope == "bad" {
			if err == nil {
				t.Fatal("invalid accepted")
			}
			continue
		}
		if err != nil || cfg.Pool.SelectionScope != scope {
			t.Fatal(scope, err)
		}
	}
	t.Setenv("WB2A_SELECTION_SCOPE", "all")
	cfg, err := Load(path)
	if err != nil || cfg.Pool.SelectionScope != "all" {
		t.Fatal("env override", err)
	}
}
