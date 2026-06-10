package config

import (
	"testing"
	"time"
)

func TestC2TReorderDurationsUseDefaults(t *testing.T) {
	cfg := &Config{}

	if got := cfg.C2TReorderDelay(); got != 35*time.Millisecond {
		t.Fatalf("C2TReorderDelay() = %v; want %v", got, 35*time.Millisecond)
	}
	if got := cfg.C2TReorderMaxDelay(); got != 250*time.Millisecond {
		t.Fatalf("C2TReorderMaxDelay() = %v; want %v", got, 250*time.Millisecond)
	}
}

func TestC2TReorderDurationsUseConfiguredValues(t *testing.T) {
	cfg := &Config{}
	cfg.Reorder.C2TDelayMs = 20
	cfg.Reorder.C2TMaxDelayMs = 100

	if got := cfg.C2TReorderDelay(); got != 20*time.Millisecond {
		t.Fatalf("C2TReorderDelay() = %v; want %v", got, 20*time.Millisecond)
	}
	if got := cfg.C2TReorderMaxDelay(); got != 100*time.Millisecond {
		t.Fatalf("C2TReorderMaxDelay() = %v; want %v", got, 100*time.Millisecond)
	}
}
