package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v2"
)

// Config is the full runtime configuration for the interest-memory service.
// Loaded from a YAML file with env-var overrides for secrets.
type Config struct {
	Server     ServerConfig     `yaml:"server"`
	LLM        LLMConfig        `yaml:"llm"`
	Embedding  EmbeddingConfig  `yaml:"embedding"`
	Fork       ForkConfig       `yaml:"fork"`
	Verify     VerifyConfig     `yaml:"verify"`
	Wiki       WikiConfig       `yaml:"wiki"`
	Search     SearchConfig     `yaml:"search"`
	Log        LogConfig        `yaml:"log"`
	Recall     RecallConfig     `yaml:"recall"`
	Worker     WorkerConfig     `yaml:"worker"`
	Namespaces NamespacesConfig `yaml:"namespaces"`
}

// Namespace modes.
const (
	NamespaceIsolated = "isolated" // default: each agent reads only its own namespace
	NamespaceAll      = "all"      // every agent reads across all namespaces
	NamespaceCustom   = "custom"   // per-agent readable set from VisibleTo
)

// NamespacesConfig controls cross-namespace visibility on the read side
// (recall / search / get). Writes are always isolated to the agent's own
// namespace. Default is isolated — behaviour identical to no configuration.
type NamespacesConfig struct {
	Mode      string              `yaml:"mode"`       // isolated | all | custom
	VisibleTo map[string][]string `yaml:"visible_to"` // custom only: agent → namespaces it may read (one-way)
}

// WorkerConfig controls the async transcript-processing worker.
type WorkerConfig struct {
	JobTimeout time.Duration `yaml:"job_timeout"` // per-job context timeout
}

// LogConfig controls the change-log retention.
type LogConfig struct {
	Retain int `yaml:"retain"` // retained log entries per agent (0 = unlimited)
}

// SearchConfig controls the consumer-side memory_search tool.
type SearchConfig struct {
	TopK       int `yaml:"top_k"`        // default number of results for semantic search
	MaxBodyLen int `yaml:"max_body_len"` // body/summary truncation cap (claims/evidence/edges not truncated)
}

// WikiConfig controls the wiki writing / reconcile stage.
type WikiConfig struct {
	Enabled   bool   `yaml:"enabled"`    // master switch: false skips wiki writes entirely (interest points only)
	MaxHops   int    `yaml:"max_hops"`   // graph propagation depth for reconciliation
	BatchSize int    `yaml:"batch_size"` // related pages per reconcile agent-loop batch
	Language  string `yaml:"language"`   // wiki page output language (default English)
}

// OutputLanguage returns the configured wiki output language, defaulting to
// English when unset.
func (c WikiConfig) OutputLanguage() string {
	if c.Language == "" {
		return "English"
	}
	return c.Language
}

// VerifyConfig controls the correction layer (verify#1 fact-checking).
type VerifyConfig struct {
	UseWebSearch   bool    `yaml:"use_web_search"`
	SearchMax      int     `yaml:"search_max"`
	WebTool        string  `yaml:"web_tool"`        // web tool name (active in the registry)
	MaxConcurrency int     `yaml:"max_concurrency"` // candidate fact-check parallelism
	MinConfidence  float64 `yaml:"min_confidence"`  // minimum confidence to store a contradiction (0~1; 0 = disabled, default disabled)
	SimThreshold   float64 `yaml:"sim_threshold"`   // semantic-grouping similarity threshold (same-topic candidate gate, default 0.45)
	MaxCandidates  int     `yaml:"max_candidates"`  // max candidate pairs per window for semantic grouping (default 30)
}

type ServerConfig struct {
	Host   string `yaml:"host"`
	Port   int    `yaml:"port"`
	DBPath string `yaml:"db_path"`
}

type LLMConfig struct {
	BaseURL   string `yaml:"base_url"`
	APIKeyEnv string `yaml:"api_key_env"`
	Model     string `yaml:"model"`
	MaxTokens int    `yaml:"max_tokens"`
}

type EmbeddingConfig struct {
	BaseURL    string `yaml:"base_url"`
	APIKeyEnv  string `yaml:"api_key_env"`
	Model      string `yaml:"model"`
	Dimensions int    `yaml:"dimensions"`
}

type ForkConfig struct {
	PrefixStep       int     `yaml:"prefix_step"`     // prefix-window step in user turns
	MaxWindows       int     `yaml:"max_windows"`     // max prefix windows (keeps the longest N when exceeded)
	MaxConcurrency   int     `yaml:"max_concurrency"` // window-extraction parallelism
	SimilarityMerge  float64 `yaml:"similarity_merge"`
	SimilarityRelate float64 `yaml:"similarity_relate"`
	ImportanceBoost  float64 `yaml:"importance_boost_per_seen"`
	MaxCandidates    int     `yaml:"max_candidates_per_window"`
	MinConfidence    float64 `yaml:"min_confidence"`
}

type RecallConfig struct {
	TopK        int     `yaml:"top_k"`
	IncludeWiki bool    `yaml:"include_wiki"`
	MinScore    float64 `yaml:"min_score"`
}

// Default returns the built-in defaults (used when no file is given).
func Default() Config {
	return Config{
		Server: ServerConfig{
			Host:   "127.0.0.1",
			Port:   8899,
			DBPath: expandHome("~/.interest-memory/memory.db"),
		},
		LLM: LLMConfig{
			BaseURL:   "https://api.openai.com/v1",
			APIKeyEnv: "LLM_API_KEY",
			Model:     "gpt-4o-mini",
			MaxTokens: 4096,
		},
		Embedding: EmbeddingConfig{
			BaseURL:    "https://api.siliconflow.cn/v1",
			APIKeyEnv:  "SILICONFLOW_API_KEY",
			Model:      "BAAI/bge-m3",
			Dimensions: 1024,
		},
		Fork: ForkConfig{
			PrefixStep:       5,
			MaxWindows:       8,
			MaxConcurrency:   4,
			SimilarityMerge:  0.85,
			SimilarityRelate: 0.50,
			ImportanceBoost:  0.05,
			MaxCandidates:    20,
			MinConfidence:    0.3,
		},
		Verify: VerifyConfig{
			UseWebSearch:   true,
			SearchMax:      5,
			WebTool:        "myagent",
			MaxConcurrency: 4,
			MinConfidence:  0,
			SimThreshold:   0.45,
			MaxCandidates:  30,
		},
		Wiki: WikiConfig{
			Enabled:   true,
			MaxHops:   3,
			BatchSize: 10,
		},
		Worker: WorkerConfig{
			// Default 45min: the wiki stage runs one agent loop per interest
			// point serially, each with its own 10min cap; 15min was too short
			// for long sessions (observed: 62-point job killed mid-wiki).
			JobTimeout: 45 * time.Minute,
		},
		Search: SearchConfig{
			TopK:       3,
			MaxBodyLen: 4000,
		},
		Log: LogConfig{
			Retain: 0,
		},
		Recall: RecallConfig{
			TopK:        8,
			IncludeWiki: true,
			MinScore:    0.30,
		},
		Namespaces: NamespacesConfig{
			Mode: NamespaceIsolated,
		},
	}
}

// Load reads a YAML config file (path == "" → defaults only), then applies
// env-var overrides for API keys. It returns the merged config.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("config: read %s: %w", path, err)
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}
	cfg.applyEnv()
	cfg.normalize()
	if err := cfg.validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// validate checks cross-cutting invariants (namespace mode, log retention).
func (c *Config) validate() error {
	switch c.Namespaces.Mode {
	case "", NamespaceIsolated, NamespaceAll, NamespaceCustom:
		// ok
	default:
		return fmt.Errorf("config: namespaces.mode must be one of isolated|all|custom (got %q)", c.Namespaces.Mode)
	}
	return nil
}

func (c *Config) applyEnv() {
	if v := os.Getenv("IM_SERVER_PORT"); v != "" {
		var port int
		if _, err := fmt.Sscanf(v, "%d", &port); err == nil && port > 0 {
			c.Server.Port = port
		}
	}
	if v := os.Getenv("IM_DB_PATH"); v != "" {
		c.Server.DBPath = v
	}
	// API keys are resolved at call time via APIKey() so the process can be
	// started without the key set (keys come from the caller's env).
}

func (c *Config) normalize() {
	c.Server.DBPath = expandHome(c.Server.DBPath)
}

// expandHome expands a leading ~/ to the user's home directory.
func expandHome(p string) string {
	if len(p) >= 2 && p[0] == '~' && p[1] == '/' {
		home, err := os.UserHomeDir()
		if err == nil {
			return home + p[1:]
		}
	}
	return p
}

// APIKey resolves the API key for a config that declares an env var name.
// Returns "" if the env var is unset.
func APIKey(envName string) string {
	if envName == "" {
		return ""
	}
	return os.Getenv(envName)
}
