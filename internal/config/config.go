package config

import (
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"AIHelper/internal/agent"
	"AIHelper/internal/gateway"
)

type Config struct {
	Agents       []agent.AgentConfig `yaml:"agents"`
	Bindings     []gateway.Binding   `yaml:"bindings"`
	LLM          LLMConfig           `yaml:"llm"`
	Channels     ChannelsConfig      `yaml:"channels"`
	Intelligence IntelligenceConfig  `yaml:"intelligence"`
	Sessions     SessionsConfig      `yaml:"sessions"`
	Delivery     DeliveryConfig      `yaml:"delivery"`
	Heartbeat    HeartbeatConfig     `yaml:"heartbeat"`
	Cron         CronConfig          `yaml:"cron"`
}

type LLMConfig struct {
	Provider       string              `yaml:"provider"`
	BaseURL        string              `yaml:"base_url"`
	APIKey         string              `yaml:"api_key"`
	DefaultModel   string              `yaml:"default_model"`
	Temperature    float64             `yaml:"temperature"`
	MaxTokens      int                 `yaml:"max_tokens"`
	Profiles       []LLMProfileConfig  `yaml:"profiles"`
	FallbackModels []string            `yaml:"fallback_models"`
	Resilience     LLMResilienceConfig `yaml:"resilience"`
}

type LLMProfileConfig struct {
	Name     string `yaml:"name"`
	Provider string `yaml:"provider"`
	BaseURL  string `yaml:"base_url"`
	APIKey   string `yaml:"api_key"`
}

type LLMResilienceConfig struct {
	Enabled                bool `yaml:"enabled"`
	MaxOverflowCompactions int  `yaml:"max_overflow_compactions"`
	ContextSafeTokens      int  `yaml:"context_safe_tokens"`
	MaxToolOutputChars     int  `yaml:"max_tool_output_chars"`
}

type ChannelsConfig struct {
	CLI    CLIConfig    `yaml:"cli"`
	Feishu FeishuConfig `yaml:"feishu"`
}

type CLIConfig struct {
	Enabled bool `yaml:"enabled"`
}

type FeishuConfig struct {
	Enabled        bool   `yaml:"enabled"`
	AccountID      string `yaml:"account_id"`
	AppID          string `yaml:"app_id"`
	AppSecret      string `yaml:"app_secret"`
	BotOpenID      string `yaml:"bot_open_id"`
	RequireMention bool   `yaml:"require_mention"`
	IsLark         bool   `yaml:"is_lark"`
}

type IntelligenceConfig struct {
	Embedding EmbeddingConfig `yaml:"embedding"`
	Plugins   PluginsConfig   `yaml:"plugins"`
}

type EmbeddingConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Provider string `yaml:"provider"`
	BaseURL  string `yaml:"base_url"`
	APIKey   string `yaml:"api_key"`
	Model    string `yaml:"model"`
}

type PluginsConfig struct {
	SkillRoots      []string `yaml:"skill_roots"`
	DisabledSkills  []string `yaml:"disabled_skills"`
	DisabledPlugins []string `yaml:"disabled_plugins"`
}

type SessionsConfig struct {
	Driver string `yaml:"driver"`
	Path   string `yaml:"path"`
}

type DeliveryConfig struct {
	Enabled             *bool  `yaml:"enabled"`
	Path                string `yaml:"path"`
	ScanIntervalSeconds int    `yaml:"scan_interval_seconds"`
	MaxRetries          int    `yaml:"max_retries"`
}

type HeartbeatConfig struct {
	Enabled         bool                   `yaml:"enabled"`
	IntervalSeconds int                    `yaml:"interval_seconds"`
	ActiveHours     ActiveHoursConfig      `yaml:"active_hours"`
	Agents          []HeartbeatAgentConfig `yaml:"agents"`
}

type HeartbeatAgentConfig struct {
	AgentID         string            `yaml:"agent_id"`
	Target          TargetConfig      `yaml:"target"`
	IntervalSeconds int               `yaml:"interval_seconds"`
	ActiveHours     ActiveHoursConfig `yaml:"active_hours"`
}

type ActiveHoursConfig struct {
	Start *int `yaml:"start"`
	End   *int `yaml:"end"`
}

type TargetConfig struct {
	Channel string `yaml:"channel"`
	PeerID  string `yaml:"peer_id"`
	ToType  string `yaml:"to_type"`
}

type CronConfig struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
}

func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}

	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return Config{}, err
	}
	applyEnvOverrides(&cfg)
	return cfg, nil
}

func applyEnvOverrides(cfg *Config) {
	if cfg == nil {
		return
	}
	if value, ok := nonEmptyEnv("AIHELPER_LLM_API_KEY"); ok {
		cfg.LLM.APIKey = value
	}
	if value, ok := nonEmptyEnv("AIHELPER_EMBEDDING_API_KEY"); ok {
		cfg.Intelligence.Embedding.APIKey = value
	}
	if value, ok := nonEmptyEnv("AIHELPER_FEISHU_APP_ID"); ok {
		cfg.Channels.Feishu.AppID = value
	}
	if value, ok := nonEmptyEnv("AIHELPER_FEISHU_APP_SECRET"); ok {
		cfg.Channels.Feishu.AppSecret = value
	}
	if value, ok := nonEmptyEnv("AIHELPER_FEISHU_BOT_OPEN_ID"); ok {
		cfg.Channels.Feishu.BotOpenID = value
	}
}

func nonEmptyEnv(key string) (string, bool) {
	value, ok := os.LookupEnv(key)
	if !ok {
		return "", false
	}
	value = strings.TrimSpace(value)
	return value, value != ""
}
