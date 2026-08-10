package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load('') error: %v", err)
	}
	if cfg.Server.Port != 8899 {
		t.Errorf("default port = %d, want 8899", cfg.Server.Port)
	}
	if cfg.Server.DBPath == "" {
		t.Error("default DBPath is empty")
	}
	if cfg.Fork.PrefixStep != 5 {
		t.Errorf("default prefix_step = %d, want 5", cfg.Fork.PrefixStep)
	}
	if cfg.Fork.MaxWindows != 8 {
		t.Errorf("default max_windows = %d, want 8", cfg.Fork.MaxWindows)
	}
	if cfg.Fork.MaxConcurrency != 4 {
		t.Errorf("default fork max_concurrency = %d, want 4", cfg.Fork.MaxConcurrency)
	}
	if cfg.Embedding.Dimensions != 1024 {
		t.Errorf("default dimensions = %d, want 1024", cfg.Embedding.Dimensions)
	}
	if cfg.Embedding.Model != "BAAI/bge-m3" {
		t.Errorf("default embedding model = %s, want BAAI/bge-m3", cfg.Embedding.Model)
	}
	if cfg.Embedding.BaseURL != "https://api.siliconflow.cn/v1" {
		t.Errorf("default embedding base_url = %s, want siliconflow", cfg.Embedding.BaseURL)
	}
	if !cfg.Verify.UseWebSearch {
		t.Error("default use_web_search = false, want true")
	}
	if cfg.Verify.SearchMax != 5 {
		t.Errorf("default search_max = %d, want 5", cfg.Verify.SearchMax)
	}
	if cfg.Verify.WebTool != "myagent" {
		t.Errorf("default web_tool = %q, want myagent", cfg.Verify.WebTool)
	}
	if cfg.Verify.MaxConcurrency != 4 {
		t.Errorf("default verify max_concurrency = %d, want 4", cfg.Verify.MaxConcurrency)
	}
	if cfg.Verify.MinConfidence != 0 {
		t.Errorf("default verify min_confidence = %f, want 0 (disabled)", cfg.Verify.MinConfidence)
	}
	if cfg.Verify.SimThreshold != 0.45 {
		t.Errorf("default verify sim_threshold = %f, want 0.45", cfg.Verify.SimThreshold)
	}
	if cfg.Verify.MaxCandidates != 30 {
		t.Errorf("default verify max_candidates = %d, want 30", cfg.Verify.MaxCandidates)
	}
	if cfg.Wiki.MaxHops != 3 || cfg.Wiki.BatchSize != 10 {
		t.Errorf("default wiki = %+v, want MaxHops=3 BatchSize=10", cfg.Wiki)
	}
	if !cfg.Wiki.Enabled {
		t.Error("default wiki.enabled = false, want true")
	}
	if cfg.Search.TopK != 3 || cfg.Search.MaxBodyLen != 4000 {
		t.Errorf("default search = %+v, want TopK=3 MaxBodyLen=4000", cfg.Search)
	}
	if cfg.Log.Retain != 0 {
		t.Errorf("default log.retain = %d, want 0 (unlimited)", cfg.Log.Retain)
	}
}

func TestLoadFileOverrides(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	content := `
server:
  port: 9999
  db_path: /tmp/custom.db
fork:
  prefix_step: 5
  similarity_merge: 0.9
verify:
  sim_threshold: 0.7
  max_candidates: 15
  min_confidence: 0.6
`
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.Server.Port != 9999 {
		t.Errorf("port = %d, want 9999", cfg.Server.Port)
	}
	if cfg.Server.DBPath != "/tmp/custom.db" {
		t.Errorf("db_path = %s, want /tmp/custom.db", cfg.Server.DBPath)
	}
	if cfg.Fork.PrefixStep != 5 {
		t.Errorf("prefix_step = %d, want 5", cfg.Fork.PrefixStep)
	}
	if cfg.Fork.SimilarityMerge != 0.9 {
		t.Errorf("similarity_merge = %f, want 0.9", cfg.Fork.SimilarityMerge)
	}
	if cfg.Verify.SimThreshold != 0.7 {
		t.Errorf("sim_threshold = %f, want 0.7", cfg.Verify.SimThreshold)
	}
	if cfg.Verify.MaxCandidates != 15 {
		t.Errorf("max_candidates = %d, want 15", cfg.Verify.MaxCandidates)
	}
	if cfg.Verify.MinConfidence != 0.6 {
		t.Errorf("min_confidence = %f, want 0.6", cfg.Verify.MinConfidence)
	}
	// Unset keys keep defaults
	if cfg.Recall.TopK != 8 {
		t.Errorf("recall.top_k = %d, want default 8", cfg.Recall.TopK)
	}
	content = `
wiki:
  enabled: false
`
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(p)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.Wiki.Enabled {
		t.Error("wiki.enabled = true, want false")
	}
}

func TestNamespacesDefaultIsolated(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Namespaces.Mode != NamespaceIsolated {
		t.Errorf("default namespaces.mode = %q, want isolated", cfg.Namespaces.Mode)
	}
	if len(cfg.Namespaces.VisibleTo) != 0 {
		t.Errorf("default visible_to should be empty, got %v", cfg.Namespaces.VisibleTo)
	}
}

func TestNamespacesLoadModes(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")

	content := `
namespaces:
  mode: all
`
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load all-mode error: %v", err)
	}
	if cfg.Namespaces.Mode != NamespaceAll {
		t.Errorf("mode = %q, want all", cfg.Namespaces.Mode)
	}

	content = `
namespaces:
  mode: custom
  visible_to:
    codex: [opencode, pi]
    claudecode: [reasonix]
`
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(p)
	if err != nil {
		t.Fatalf("Load custom-mode error: %v", err)
	}
	if cfg.Namespaces.Mode != NamespaceCustom {
		t.Errorf("mode = %q, want custom", cfg.Namespaces.Mode)
	}
	if got := cfg.Namespaces.VisibleTo["codex"]; len(got) != 2 || got[0] != "opencode" || got[1] != "pi" {
		t.Errorf("visible_to[codex] = %v, want [opencode pi]", got)
	}
	if got := cfg.Namespaces.VisibleTo["claudecode"]; len(got) != 1 || got[0] != "reasonix" {
		t.Errorf("visible_to[claudecode] = %v, want [reasonix]", got)
	}
}

func TestNamespacesInvalidModeRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	content := `
namespaces:
  mode: everything
`
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for invalid namespaces.mode")
	}
}

func TestExpandHome(t *testing.T) {
	home, _ := os.UserHomeDir()
	got := expandHome("~/x/y.db")
	want := home + "/x/y.db"
	if got != want {
		t.Errorf("expandHome = %s, want %s", got, want)
	}
	if expandHome("/abs/path") != "/abs/path" {
		t.Error("expandHome should leave absolute paths unchanged")
	}
}

func TestAPIKey(t *testing.T) {
	os.Setenv("IM_TEST_KEY", "secret")
	defer os.Unsetenv("IM_TEST_KEY")
	if got := APIKey("IM_TEST_KEY"); got != "secret" {
		t.Errorf("APIKey = %s, want secret", got)
	}
	if got := APIKey(""); got != "" {
		t.Errorf("APIKey('') = %s, want empty", got)
	}
}
