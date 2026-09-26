package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Upload limits across an owner (a person's namespace or a team's). The
// intent is that a person accumulates hundreds of artifacts, so the site cap
// is far above that; the byte cap bounds the bucket (ten thousand owners at
// the cap is 100 TiB); five retained versions per site is what the server
// kept before these settings existed.
const (
	defaultQuotaMaxSites    = 1000
	defaultQuotaMaxBytes    = 10 << 30 // 10 GiB
	defaultQuotaMaxVersions = 5
	maxQuotaMaxVersions     = 100
	defaultClamdTimeout     = 30 * time.Second
)

// QuotaConfig is QUOTA_MAX_SITES, QUOTA_MAX_BYTES and QUOTA_MAX_VERSIONS.
// Zero MaxSites or MaxBytes means unlimited.
type QuotaConfig struct {
	MaxSites    int64
	MaxBytes    int64
	MaxVersions int
}

// ClamdConfig is the optional malware scan. An empty Addr means uploads are
// not scanned.
type ClamdConfig struct {
	Addr    string
	Timeout time.Duration
}

func loadUploadLimits() (QuotaConfig, ClamdConfig, error) {
	var quota QuotaConfig
	var err error
	if quota.MaxSites, err = nonNegativeInt64Env("QUOTA_MAX_SITES", defaultQuotaMaxSites); err != nil {
		return QuotaConfig{}, ClamdConfig{}, err
	}
	if quota.MaxBytes, err = nonNegativeInt64Env("QUOTA_MAX_BYTES", defaultQuotaMaxBytes); err != nil {
		return QuotaConfig{}, ClamdConfig{}, err
	}
	versions, err := int64Env("QUOTA_MAX_VERSIONS", defaultQuotaMaxVersions)
	if err != nil {
		return QuotaConfig{}, ClamdConfig{}, err
	}
	if versions > maxQuotaMaxVersions {
		return QuotaConfig{}, ClamdConfig{}, fmt.Errorf("QUOTA_MAX_VERSIONS must be 1 to %d, got %d", maxQuotaMaxVersions, versions)
	}
	quota.MaxVersions = int(versions)

	clamd := ClamdConfig{Addr: strings.TrimSpace(os.Getenv("CLAMD_ADDR"))}
	if clamd.Addr != "" {
		host, port, err := net.SplitHostPort(clamd.Addr)
		if err != nil || host == "" {
			return QuotaConfig{}, ClamdConfig{}, fmt.Errorf("CLAMD_ADDR must be host:port, got %q", clamd.Addr)
		}
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return QuotaConfig{}, ClamdConfig{}, fmt.Errorf("CLAMD_ADDR port must be 1 to 65535, got %q", port)
		}
	}
	if clamd.Timeout, err = durationEnv("CLAMD_TIMEOUT", defaultClamdTimeout); err != nil {
		return QuotaConfig{}, ClamdConfig{}, err
	}
	return quota, clamd, nil
}

// nonNegativeInt64Env is int64Env where 0 is allowed (and means unlimited).
func nonNegativeInt64Env(key string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if value < 0 {
		return 0, fmt.Errorf("%s must be 0 (unlimited) or positive, got %s", key, raw)
	}
	return value, nil
}
