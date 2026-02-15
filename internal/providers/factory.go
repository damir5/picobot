package providers

import (
	"time"

	"github.com/local/picobot/internal/config"
)

// NewProviderFromConfig creates a provider based on the configuration.
// Simple rules (v0):
//   - if OpenAI API key present -> OpenAI
//   - else fallback to stub
func NewProviderFromConfig(cfg config.Config) LLMProvider {
	if cfg.Providers.OpenAI != nil && cfg.Providers.OpenAI.APIKey != "" {
		timeout := time.Duration(cfg.Agents.Defaults.TimeoutS) * time.Second
		return NewOpenAIProvider(cfg.Providers.OpenAI.APIKey, cfg.Providers.OpenAI.APIBase, timeout, cfg.Agents.Defaults.MaxRetries)
	}
	return NewStubProvider()
}
