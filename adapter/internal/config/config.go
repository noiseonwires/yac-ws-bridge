package config

import (
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds all adapter configuration.
type Config struct {
	Bridge struct {
		URL       string `yaml:"url"`
		AuthToken string `yaml:"authToken"`
		PoolSize  int    `yaml:"poolSize"`
		Reconnect struct {
			InitialDelayMs    int     `yaml:"initialDelayMs"`
			MaxDelayMs        int     `yaml:"maxDelayMs"`
			BackoffMultiplier float64 `yaml:"backoffMultiplier"`
		} `yaml:"reconnect"`
		PingIntervalMs int `yaml:"pingIntervalMs"`
	} `yaml:"bridge"`
	Wakeup struct {
		ListenPort int    `yaml:"listenPort"`
		PathPrefix string `yaml:"pathPrefix"`
		TLSCert    string `yaml:"tlsCert"`
		TLSKey     string `yaml:"tlsKey"`
	} `yaml:"wakeup"`
	Target struct {
		URL string `yaml:"url"`
	} `yaml:"target"`
	WsApi struct {
		Mode string `yaml:"mode"` // "grpc" or "rest", default "rest"
	} `yaml:"wsApi"`
	Logging struct {
		Level string `yaml:"level"`
	} `yaml:"logging"`
}

// InitialDelay returns the reconnect initial delay as a time.Duration.
func (c *Config) InitialDelay() time.Duration {
	return time.Duration(c.Bridge.Reconnect.InitialDelayMs) * time.Millisecond
}

// MaxDelay returns the reconnect max delay as a time.Duration.
func (c *Config) MaxDelay() time.Duration {
	return time.Duration(c.Bridge.Reconnect.MaxDelayMs) * time.Millisecond
}

// PingInterval returns the ping interval as a time.Duration.
func (c *Config) PingInterval() time.Duration {
	return time.Duration(c.Bridge.PingIntervalMs) * time.Millisecond
}

// Load reads a YAML config file and returns a Config.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
