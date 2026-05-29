package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAppliesEnvOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte(`
llm:
  api_key: yaml-llm-key
intelligence:
  embedding:
    api_key: yaml-embedding-key
channels:
  feishu:
    app_id: yaml-app-id
    app_secret: yaml-secret
    bot_open_id: yaml-bot
`)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	t.Setenv("AIHELPER_LLM_API_KEY", "env-llm-key")
	t.Setenv("AIHELPER_EMBEDDING_API_KEY", "env-embedding-key")
	t.Setenv("AIHELPER_FEISHU_APP_ID", "env-app-id")
	t.Setenv("AIHELPER_FEISHU_APP_SECRET", "env-secret")
	t.Setenv("AIHELPER_FEISHU_BOT_OPEN_ID", "env-bot")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.LLM.APIKey != "env-llm-key" {
		t.Fatalf("llm api key = %q", cfg.LLM.APIKey)
	}
	if cfg.Intelligence.Embedding.APIKey != "env-embedding-key" {
		t.Fatalf("embedding api key = %q", cfg.Intelligence.Embedding.APIKey)
	}
	if cfg.Channels.Feishu.AppID != "env-app-id" {
		t.Fatalf("feishu app id = %q", cfg.Channels.Feishu.AppID)
	}
	if cfg.Channels.Feishu.AppSecret != "env-secret" {
		t.Fatalf("feishu app secret = %q", cfg.Channels.Feishu.AppSecret)
	}
	if cfg.Channels.Feishu.BotOpenID != "env-bot" {
		t.Fatalf("feishu bot open id = %q", cfg.Channels.Feishu.BotOpenID)
	}
}

func TestLoadIgnoresEmptyEnvOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte(`
llm:
  api_key: yaml-llm-key
channels:
  feishu:
    app_secret: yaml-secret
`)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	t.Setenv("AIHELPER_LLM_API_KEY", " ")
	t.Setenv("AIHELPER_FEISHU_APP_SECRET", "")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.LLM.APIKey != "yaml-llm-key" {
		t.Fatalf("llm api key = %q", cfg.LLM.APIKey)
	}
	if cfg.Channels.Feishu.AppSecret != "yaml-secret" {
		t.Fatalf("feishu app secret = %q", cfg.Channels.Feishu.AppSecret)
	}
}

func TestDevConfigsAllowWriteFileForAllAgents(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", "configs", "dev.yaml"),
		filepath.Join("..", "..", "configs", "dev.example.yaml"),
	} {
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("load %s: %v", path, err)
		}
		if len(cfg.Agents) == 0 {
			t.Fatalf("%s has no agents", path)
		}
		for _, agent := range cfg.Agents {
			if !containsString(agent.Tools, "write_file") {
				t.Fatalf("%s agent %q tools missing write_file: %#v", path, agent.ID, agent.Tools)
			}
		}
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
