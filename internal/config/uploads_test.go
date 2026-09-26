package config

import (
	"testing"
	"time"
)

func TestLoadUploadLimitDefaults(t *testing.T) {
	completeEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := QuotaConfig{MaxSites: 1000, MaxBytes: 10 << 30, MaxVersions: 5}
	if cfg.Quota != want {
		t.Fatalf("Quota = %+v, want %+v", cfg.Quota, want)
	}
	if cfg.Clamd.Addr != "" || cfg.Clamd.Timeout != 30*time.Second {
		t.Fatalf("Clamd = %+v, want unset with a 30s timeout", cfg.Clamd)
	}
}

func TestLoadUploadLimitOverrides(t *testing.T) {
	completeEnv(t)
	t.Setenv("QUOTA_MAX_SITES", "0")
	t.Setenv("QUOTA_MAX_BYTES", "0")
	t.Setenv("QUOTA_MAX_VERSIONS", "20")
	t.Setenv("CLAMD_ADDR", "clamd.simple-host.svc:3310")
	t.Setenv("CLAMD_TIMEOUT", "5s")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Quota != (QuotaConfig{MaxSites: 0, MaxBytes: 0, MaxVersions: 20}) {
		t.Fatalf("Quota = %+v", cfg.Quota)
	}
	if cfg.Clamd != (ClamdConfig{Addr: "clamd.simple-host.svc:3310", Timeout: 5 * time.Second}) {
		t.Fatalf("Clamd = %+v", cfg.Clamd)
	}
}

func TestLoadRejectsInvalidUploadLimits(t *testing.T) {
	for name, env := range map[string][2]string{
		"negative sites":    {"QUOTA_MAX_SITES", "-1"},
		"negative bytes":    {"QUOTA_MAX_BYTES", "-5"},
		"bytes not numeric": {"QUOTA_MAX_BYTES", "10GiB"},
		"zero versions":     {"QUOTA_MAX_VERSIONS", "0"},
		"too many versions": {"QUOTA_MAX_VERSIONS", "101"},
		"clamd no port":     {"CLAMD_ADDR", "clamd"},
		"clamd bad port":    {"CLAMD_ADDR", "clamd:99999"},
		"clamd no host":     {"CLAMD_ADDR", ":3310"},
		"clamd timeout":     {"CLAMD_TIMEOUT", "0s"},
	} {
		t.Run(name, func(t *testing.T) {
			completeEnv(t)
			t.Setenv(env[0], env[1])
			if _, err := Load(); err == nil {
				t.Fatalf("%s=%s accepted", env[0], env[1])
			}
		})
	}
}
