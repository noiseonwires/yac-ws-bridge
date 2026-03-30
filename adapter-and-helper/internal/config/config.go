package config

import (
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Bridge struct {
		URL       string `yaml:"url"`
		AuthToken string `yaml:"authToken"`
		Reconnect struct {
			InitialDelayMs    int     `yaml:"initialDelayMs"`
			MaxDelayMs        int     `yaml:"maxDelayMs"`
			BackoffMultiplier float64 `yaml:"backoffMultiplier"`
		} `yaml:"reconnect"`
		PingIntervalMs int `yaml:"pingIntervalMs"`
	} `yaml:"bridge"`
	Target struct {
		Address string `yaml:"address"`
	} `yaml:"target"`
	HTTP struct {
		ListenPort int `yaml:"listenPort"`
	} `yaml:"http"`
	Listen struct {
		Address string `yaml:"address"`
	} `yaml:"listen"`
	WsAPI struct {
		Mode  string `yaml:"mode"`
		Relay bool   `yaml:"relay"`
	} `yaml:"wsApi"`
	WriteCoalescing struct {
		Enabled bool `yaml:"enabled"`
		DelayMs int  `yaml:"delayMs"`
	} `yaml:"writeCoalescing"`
	Logging struct {
		Level string `yaml:"level"`
	} `yaml:"logging"`
}

func (c *Config) InitialDelay() time.Duration {
	return time.Duration(c.Bridge.Reconnect.InitialDelayMs) * time.Millisecond
}

func (c *Config) MaxDelay() time.Duration {
	return time.Duration(c.Bridge.Reconnect.MaxDelayMs) * time.Millisecond
}

func (c *Config) PingInterval() time.Duration {
	return time.Duration(c.Bridge.PingIntervalMs) * time.Millisecond
}

func (c *Config) CoalesceDelay() time.Duration {
	if !c.WriteCoalescing.Enabled || c.WriteCoalescing.DelayMs <= 0 {
		return 0
	}
	return time.Duration(c.WriteCoalescing.DelayMs) * time.Millisecond
}

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
