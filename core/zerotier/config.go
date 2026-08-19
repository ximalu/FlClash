package zerotier

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// File names inside the app home dir (constant.Path.HomeDir() on Android).
const (
	ConfigFileName   = "zerotier.json"
	IdentityFileName = "zerotier-identity.secret"

	// DefaultPort is the default ZeroTier wire UDP port.
	//
	// 9994 (not 9993) matches the official ZeroTier Android client, which
	// deliberately uses 9994 so it does not collide with a second ZeroTier
	// instance on the same device (desktop daemons bind 9993). Choosing the
	// same default as the official Android client also means our port is
	// already battle-tested against the same NAT/firewall patterns.
	//
	// IMPORTANT: the port is an invariant. A bind failure must NEVER fall
	// back to a random port (P0-1): silently changing the endpoint breaks
	// every peer's learned path and leaves the node unreachable (observed
	// 2026-08-19: i3 saw the node as RELAY -1 after a random-port fallback).
	DefaultPort = 9994
)

// Config is the content of <homeDir>/zerotier.json.
//
//	{
//	  "network-id": "b6079f73c6c0eb31",
//	  "port": 0
//	}
//
// port is the local UDP port for ZeroTier wire traffic (0 = default 9993,
// fallback to an ephemeral port if 9993 is busy). Omit for default.
type Config struct {
	NetworkID string `json:"network-id"`
	Port      int    `json:"port,omitempty"`
}

// LoadConfig reads <homeDir>/zerotier.json. A missing file means ZeroTier is
// disabled: (nil, nil). Parse/IO errors are returned so the caller can fall
// back to the plain mihomo pump.
func LoadConfig(homeDir string) (*Config, error) {
	if homeDir == "" {
		return nil, nil
	}
	p := filepath.Join(homeDir, ConfigFileName)
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	cfg := &Config{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	cfg.NetworkID = strings.TrimSpace(cfg.NetworkID)
	return cfg, nil
}

// Enabled reports whether ZeroTier should be started (network-id present).
func (c *Config) Enabled() bool { return c != nil && c.NetworkID != "" }

// ParseNWID converts a 16-hex-digit network ID (with or without 0x prefix).
func ParseNWID(s string) (uint64, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "0x")
	if len(s) != 16 {
		return 0, errors.New("zerotier: network-id must be 16 hex digits")
	}
	v, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		return 0, err
	}
	if v == 0 {
		return 0, errors.New("zerotier: network-id cannot be zero")
	}
	return v, nil
}
