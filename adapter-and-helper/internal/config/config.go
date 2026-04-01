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
	QUIC struct {
		MTU         int `yaml:"mtu"`
		SendWorkers int `yaml:"sendWorkers"`
	} `yaml:"quic"`
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

func (c *Config) MTU() int {
	if c.QUIC.MTU <= 0 {
		return 1200
	}
	return c.QUIC.MTU
}

func (c *Config) SendWorkers() int {
	if c.QUIC.SendWorkers <= 0 {
		return 8
	}
	return c.QUIC.SendWorkers
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
