package config

import (
	"fmt"
	"net/netip"
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

	// Tun describes the local TUN interface created by this process.
	Tun struct {
		// Name is the requested device name. On Linux: "btf0". On Windows:
		// the wintun adapter display name. On macOS this is best left empty
		// (utun assigns one).
		Name string `yaml:"name"`
		// Address is the local tunnel IP, e.g. "10.200.0.2".
		Address string `yaml:"address"`
		// PeerAddress is the remote tunnel IP, e.g. "10.200.0.1".
		PeerAddress string `yaml:"peerAddress"`
		// MTU; defaults to 1400 if zero/missing.
		MTU int `yaml:"mtu"`
	} `yaml:"tun"`

	HTTP struct {
		ListenPort int `yaml:"listenPort"`
	} `yaml:"http"`

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

// TunAddress parses tun.address.
func (c *Config) TunAddress() (netip.Addr, error) {
	if c.Tun.Address == "" {
		return netip.Addr{}, fmt.Errorf("tun.address is required")
	}
	return netip.ParseAddr(c.Tun.Address)
}

// TunPeerAddress parses tun.peerAddress.
func (c *Config) TunPeerAddress() (netip.Addr, error) {
	if c.Tun.PeerAddress == "" {
		return netip.Addr{}, fmt.Errorf("tun.peerAddress is required")
	}
	return netip.ParseAddr(c.Tun.PeerAddress)
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
