package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v2"
)

// Config is the full runtime configuration for the interest-memory service.
// Loaded from a YAML file with env-var overrides for secrets.
type Config struct {
	Server    ServerConfig    `yaml:"server"`
	LLM       LLMConfig       `yaml:"llm"`
	Embedding EmbeddingConfig `yaml:"embedding"`
	Fork      ForkConfig      `yaml:"fork"`
	Verify    VerifyConfig    `yaml:"verify"`
	Wiki      WikiConfig      `yaml:"wiki"`
	Recall    RecallConfig    `yaml:"recall"`
}

// WikiConfig controls the wiki writing / reconcile stage.
type WikiConfig struct {
	MaxHops   int `yaml:"max_hops"`   // 协同图传播深度
	BatchSize int `yaml:"batch_size"` // 协同 agent loop 每批相关页数
}

// VerifyConfig controls the correction layer (verify#1 fact-checking).
type VerifyConfig struct {
	UseWebSearch   bool   `yaml:"use_web_search"`
	SearchMax      int    `yaml:"search_max"`
	WebTool        string `yaml:"web_tool"`        // 网络工具名（registry 中 active）
	MaxConcurrency int    `yaml:"max_concurrency"` // 候选核查并行度
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
	PrefixStep       int     `yaml:"prefix_step"`       // 前缀窗口按 user 回合的递增步长
	MaxWindows       int     `yaml:"max_windows"`       // 前缀窗口上限（超限保留最长的 N 个）
	MaxConcurrency   int     `yaml:"max_concurrency"`   // 窗口提取并行度
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
		},
		Wiki: WikiConfig{
			MaxHops:   3,
			BatchSize: 10,
		},
		Recall: RecallConfig{
			TopK:        8,
			IncludeWiki: true,
			MinScore:    0.30,
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
	return cfg, nil
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
