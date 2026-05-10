package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const configFileName = "config.yaml"

// Config is persisted in <probeDir>/config.yaml. An empty Model means "use
// the provider's built-in default", so configs stay portable when SDK
// defaults shift.
type Config struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model,omitempty"`
}

func configPath(probeDir string) string {
	return filepath.Join(probeDir, configFileName)
}

// LoadConfig reads <probeDir>/config.yaml. Returns os.ErrNotExist when the
// file is missing so callers can offer a useful hint.
func LoadConfig(probeDir string) (Config, error) {
	data, err := os.ReadFile(configPath(probeDir))
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parsing %s: %w", configPath(probeDir), err)
	}
	return cfg, nil
}

// WriteConfig serializes cfg to <probeDir>/config.yaml.
func WriteConfig(probeDir string, cfg Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(probeDir), data, 0644)
}

// configExists reports whether config.yaml is present in probeDir.
func configExists(probeDir string) bool {
	_, err := os.Stat(configPath(probeDir))
	return err == nil || !errors.Is(err, os.ErrNotExist)
}
