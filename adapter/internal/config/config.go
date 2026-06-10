package config

import (
	"os"
	"strings"
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
	Reorder struct {
		C2TDelayMs    int    `yaml:"c2tDelayMs"`
		C2TMaxDelayMs int    `yaml:"c2tMaxDelayMs"`
		// SeqOrder controls how DATA_C2T sequence IDs (Yandex API Gateway message
		// IDs) map to chronological order. "descending" (default) means the newest
		// message has the smallest ID (Yandex's reverse-chronological scheme);
		// "ascending" means newest = largest. Make this configurable so a change in
		// Yandex behaviour can be handled without rebuilding.
		SeqOrder string `yaml:"seqOrder"`
	} `yaml:"reorder"`
	Logging struct {
		Level string `yaml:"level"`
	} `yaml:"logging"`
}

const (
	DefaultC2TReorderDelayMs    = 35
	DefaultC2TReorderMaxDelayMs = 250
)

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

// C2TReorderDelay returns the client-to-target reorder delay.
func (c *Config) C2TReorderDelay() time.Duration {
	if c.Reorder.C2TDelayMs <= 0 {
		return DefaultC2TReorderDelayMs * time.Millisecond
	}
	return time.Duration(c.Reorder.C2TDelayMs) * time.Millisecond
}

// C2TReorderMaxDelay returns the maximum client-to-target reorder delay.
func (c *Config) C2TReorderMaxDelay() time.Duration {
	if c.Reorder.C2TMaxDelayMs <= 0 {
		return DefaultC2TReorderMaxDelayMs * time.Millisecond
	}
	return time.Duration(c.Reorder.C2TMaxDelayMs) * time.Millisecond
}

// C2TSeqDescending reports whether DATA_C2T sequence IDs sort chronologically in
// DESCENDING lexicographic order (newest message = smallest ID). This is
// Yandex's current reverse-chronological behaviour and the default. Set
// reorder.seqOrder: "ascending" if Yandex ever switches to forward-chronological
// message IDs (newest = largest). Unknown/empty values default to descending.
func (c *Config) C2TSeqDescending() bool {
	switch strings.ToLower(strings.TrimSpace(c.Reorder.SeqOrder)) {
	case "ascending", "asc", "forward":
		return false
	default:
		return true
	}
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
