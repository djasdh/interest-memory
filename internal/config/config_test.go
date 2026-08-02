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
	if cfg.Wiki.MaxHops != 3 || cfg.Wiki.BatchSize != 10 {
		t.Errorf("default wiki = %+v, want MaxHops=3 BatchSize=10", cfg.Wiki)
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
	// Unset keys keep defaults
	if cfg.Recall.TopK != 8 {
		t.Errorf("recall.top_k = %d, want default 8", cfg.Recall.TopK)
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
