package config

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// LoadConfig loads config from ~/.picobot/config.yaml if present, otherwise returns defaults.
func LoadConfig() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	path := filepath.Join(home, ".picobot", "config.yaml")
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		// return empty config (not an error)
		return Config{}, nil
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
