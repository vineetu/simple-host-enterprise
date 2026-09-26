package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadOwnerCerts(t *testing.T) {
	completeEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OwnerCerts != "auto" {
		t.Fatalf("OwnerCerts = %q, want the default auto", cfg.OwnerCerts)
	}
	t.Setenv("OWNER_CERTS", "manual")
	if cfg, err = Load(); err != nil || cfg.OwnerCerts != "manual" {
		t.Fatalf("OWNER_CERTS=manual: %q, %v", cfg.OwnerCerts, err)
	}
	for _, bad := range []string{"Auto", "off", "true"} {
		t.Setenv("OWNER_CERTS", bad)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "OWNER_CERTS") {
			t.Errorf("OWNER_CERTS=%q: err = %v, want a refusal naming OWNER_CERTS", bad, err)
		}
	}
}

func ownerHostsEnv(t *testing.T) {
	t.Helper()
	completeEnv(t)
	t.Setenv("OWNER_CERT_ISSUER", "letsencrypt")
	t.Setenv("OWNER_INGRESS_TEMPLATE", "")
	t.Setenv("OWNER_HOSTS_INTERVAL", "")
}

func TestLoadOwnerHosts(t *testing.T) {
	ownerHostsEnv(t)
	cfg, err := LoadOwnerHosts()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicBaseURL != "https://example.com/" || cfg.Issuer != "letsencrypt" ||
		cfg.TemplateIngress != "simple-host" || cfg.Interval != 15*time.Second || cfg.DSN == "" {
		t.Fatalf("LoadOwnerHosts = %+v", cfg)
	}

	t.Setenv("OWNER_INGRESS_TEMPLATE", "my-ingress")
	t.Setenv("OWNER_HOSTS_INTERVAL", "1m")
	if cfg, err = LoadOwnerHosts(); err != nil || cfg.TemplateIngress != "my-ingress" || cfg.Interval != time.Minute {
		t.Fatalf("overrides: %+v, %v", cfg, err)
	}
	t.Setenv("OWNER_HOSTS_INTERVAL", "soon")
	if _, err := LoadOwnerHosts(); err == nil {
		t.Fatal("LoadOwnerHosts accepted an unparseable interval")
	}
}

func TestLoadOwnerHostsRequiresBaseURLAndIssuer(t *testing.T) {
	ownerHostsEnv(t)
	t.Setenv("PUBLIC_BASE_URL", "")
	t.Setenv("OWNER_CERT_ISSUER", "")
	_, err := LoadOwnerHosts()
	if err == nil {
		t.Fatal("LoadOwnerHosts succeeded with nothing configured")
	}
	for _, want := range []string{"PUBLIC_BASE_URL", "OWNER_CERT_ISSUER"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

// The reconciler connects to the database under the same TLS rules as the
// server.
func TestLoadOwnerHostsRefusesWeakDatabaseTLS(t *testing.T) {
	ownerHostsEnv(t)
	t.Setenv("DB_DSN", "postgres://example.invalid/simplehost?sslmode=disable")
	if _, err := LoadOwnerHosts(); err == nil {
		t.Fatal("LoadOwnerHosts accepted sslmode=disable")
	}
}
