package main

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// yamlConfig is the on-disk YAML shape. Pointers/omitempty keep output tidy and
// let us detect "unset" so defaults apply.
type yamlConfig struct {
	Telegram struct {
		BotToken         string   `yaml:"bot_token"`
		AllowedUserIDs   []int64  `yaml:"allowed_user_ids"`
		AllowedUsernames []string `yaml:"allowed_usernames"`
	} `yaml:"telegram"`
	Models struct {
		Manager       string `yaml:"manager"`
		Worker        string `yaml:"worker"`
		ManagerAlways *bool  `yaml:"manager_always"`
	} `yaml:"models"`
	Claude struct {
		Path       string `yaml:"path"`
		OauthToken string `yaml:"oauth_token"`
	} `yaml:"claude"`
	Backend struct {
		Default           string `yaml:"default"`
		CodexPath         string `yaml:"codex_path"`
		CodexModel        string `yaml:"codex_model"`
		CodexManagerModel string `yaml:"codex_manager_model"`
	} `yaml:"backend"`
	Runtime struct {
		TimeoutMinutes      *int `yaml:"timeout_minutes"`
		MaxWorkers          *int `yaml:"max_workers"`
		RateLimitPerMin     *int `yaml:"rate_limit_per_min"`
		ConversationTTLDays *int `yaml:"conversation_ttl_days"`
	} `yaml:"runtime"`
	Scripts struct {
		Allow           bool     `yaml:"allow"`
		AllowedCommands []string `yaml:"allowed_commands"`
	} `yaml:"scripts"`
	ScreenControl struct {
		Enabled     bool   `yaml:"enabled"`
		PresetsFile string `yaml:"presets_file"`
		Elevated    bool   `yaml:"elevated"`
	} `yaml:"screen_control"`
}

// defaults mirror config.go LoadConfig defaults.
func yamlToConfig(y *yamlConfig) *Config {
	c := &Config{
		ManagerModel:        "haiku",
		TimeoutMinutes:      10,
		ManagerAlways:       true,
		MaxWorkers:          3,
		RateLimitPerMin:     20,
		AllowScripts:        false,
		ConversationTTLDays: 30,
	}
	c.TelegramBotToken = y.Telegram.BotToken
	c.AllowedUserIDs = y.Telegram.AllowedUserIDs
	for _, u := range y.Telegram.AllowedUsernames {
		if name := strings.TrimPrefix(strings.TrimSpace(u), "@"); name != "" {
			c.AllowedUsernames = append(c.AllowedUsernames, name)
		}
	}
	if y.Models.Manager != "" {
		c.ManagerModel = y.Models.Manager
	}
	c.WorkerModel = y.Models.Worker
	if y.Models.ManagerAlways != nil {
		c.ManagerAlways = *y.Models.ManagerAlways
	}
	c.ClaudePath = y.Claude.Path
	c.ClaudeOauthToken = y.Claude.OauthToken
	c.DefaultBackend = strings.ToLower(y.Backend.Default)
	c.CodexPath = y.Backend.CodexPath
	c.CodexModel = y.Backend.CodexModel
	c.CodexManagerModel = y.Backend.CodexManagerModel
	if y.Runtime.TimeoutMinutes != nil {
		c.TimeoutMinutes = *y.Runtime.TimeoutMinutes
	}
	if y.Runtime.MaxWorkers != nil {
		c.MaxWorkers = *y.Runtime.MaxWorkers
	}
	if y.Runtime.RateLimitPerMin != nil {
		c.RateLimitPerMin = *y.Runtime.RateLimitPerMin
	}
	if y.Runtime.ConversationTTLDays != nil {
		c.ConversationTTLDays = *y.Runtime.ConversationTTLDays
	}
	c.AllowScripts = y.Scripts.Allow
	for _, cmd := range y.Scripts.AllowedCommands {
		if s := strings.TrimSpace(cmd); s != "" {
			c.AllowedScriptCommands = append(c.AllowedScriptCommands, s)
		}
	}
	c.ScreenControl = y.ScreenControl.Enabled
	c.ScreenPresetsFile = y.ScreenControl.PresetsFile
	c.ScreenElevated = y.ScreenControl.Elevated
	return c
}

func configToYAML(c *Config) *yamlConfig {
	y := &yamlConfig{}
	y.Telegram.BotToken = c.TelegramBotToken
	y.Telegram.AllowedUserIDs = c.AllowedUserIDs
	y.Telegram.AllowedUsernames = c.AllowedUsernames
	y.Models.Manager = c.ManagerModel
	y.Models.Worker = c.WorkerModel
	ma := c.ManagerAlways
	y.Models.ManagerAlways = &ma
	y.Claude.Path = c.ClaudePath
	y.Claude.OauthToken = c.ClaudeOauthToken
	y.Backend.Default = c.DefaultBackend
	y.Backend.CodexPath = c.CodexPath
	y.Backend.CodexModel = c.CodexModel
	y.Backend.CodexManagerModel = c.CodexManagerModel
	tm, mw, rl, ttl := c.TimeoutMinutes, c.MaxWorkers, c.RateLimitPerMin, c.ConversationTTLDays
	y.Runtime.TimeoutMinutes = &tm
	y.Runtime.MaxWorkers = &mw
	y.Runtime.RateLimitPerMin = &rl
	y.Runtime.ConversationTTLDays = &ttl
	y.Scripts.Allow = c.AllowScripts
	y.Scripts.AllowedCommands = c.AllowedScriptCommands
	y.ScreenControl.Enabled = c.ScreenControl
	y.ScreenControl.PresetsFile = c.ScreenPresetsFile
	y.ScreenControl.Elevated = c.ScreenElevated
	return y
}

func marshalConfigYAML(c *Config) ([]byte, error) {
	return yaml.Marshal(configToYAML(c))
}

// unmarshalConfigYAML parses YAML, applies defaults, and validates.
func unmarshalConfigYAML(b []byte) (*Config, error) {
	var y yamlConfig
	if err := yaml.Unmarshal(b, &y); err != nil {
		return nil, fmt.Errorf("config.yaml 파싱 실패: %w", err)
	}
	c := yamlToConfig(&y)
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}
