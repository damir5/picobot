package config

// Config holds picobot configuration (minimal for v0).
type Config struct {
	Agents    AgentsConfig    `json:"agents" yaml:"agents"`
	Channels  ChannelsConfig  `json:"channels" yaml:"channels"`
	Providers ProvidersConfig `json:"providers" yaml:"providers"`
}

type AgentsConfig struct {
	Defaults AgentDefaults `json:"defaults" yaml:"defaults"`
}

type AgentDefaults struct {
	Workspace          string  `json:"workspace" yaml:"workspace"`
	Model              string  `json:"model" yaml:"model"`
	MaxTokens          int     `json:"maxTokens" yaml:"maxTokens"`
	Temperature        float64 `json:"temperature" yaml:"temperature"`
	MaxToolIterations  int     `json:"maxToolIterations" yaml:"maxToolIterations"`
	HeartbeatIntervalS int     `json:"heartbeatIntervalS" yaml:"heartbeatIntervalS"`
}

type ChannelsConfig struct {
	Telegram TelegramConfig `json:"telegram" yaml:"telegram"`
}

type TelegramConfig struct {
	Enabled   bool     `json:"enabled" yaml:"enabled"`
	Token     string   `json:"token" yaml:"token"`
	AllowFrom []string `json:"allowFrom" yaml:"allowFrom"`
}

type ProvidersConfig struct {
	OpenAI *ProviderConfig `json:"openai,omitempty" yaml:"openai,omitempty"`
}

type ProviderConfig struct {
	APIKey  string `json:"apiKey" yaml:"apiKey"`
	APIBase string `json:"apiBase" yaml:"apiBase"`
}
