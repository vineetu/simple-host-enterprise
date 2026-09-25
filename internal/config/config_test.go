package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidatePublicBaseURL(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		secureMode bool
		want       string
		wantErr    bool
	}{
		{name: "secure origin", raw: "https://example.com/", secureMode: true, want: "https://example.com"},
		{name: "local HTTP origin", raw: "http://localhost:8080/", want: "http://localhost:8080"},
		{name: "HTTP forbidden in secure mode", raw: "http://example.com", secureMode: true, wantErr: true},
		{name: "missing scheme", raw: "example.com", secureMode: true, wantErr: true},
		{name: "missing host", raw: "https:///", secureMode: true, wantErr: true},
		{name: "empty hostname with port", raw: "https://:443", secureMode: true, wantErr: true},
		{name: "credentials", raw: "https://user:pass@example.com", secureMode: true, wantErr: true},
		{name: "query", raw: "https://example.com?next=elsewhere", secureMode: true, wantErr: true},
		{name: "empty query", raw: "https://example.com?", secureMode: true, wantErr: true},
		{name: "fragment", raw: "https://example.com/#fragment", secureMode: true, wantErr: true},
		{name: "empty fragment", raw: "https://example.com/#", secureMode: true, wantErr: true},
		{name: "subpath", raw: "https://example.com/simple-host", secureMode: true, wantErr: true},
		{name: "encoded subpath", raw: "https://example.com/%2f", secureMode: true, wantErr: true},
		{name: "unsupported scheme", raw: "ftp://example.com", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := validatePublicBaseURL(test.raw, test.secureMode)
			if test.wantErr {
				if err == nil {
					t.Fatalf("validatePublicBaseURL(%q, %t) = %q, want error", test.raw, test.secureMode, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("validatePublicBaseURL(%q, %t): %v", test.raw, test.secureMode, err)
			}
			if got != test.want {
				t.Fatalf("validatePublicBaseURL(%q, %t) = %q, want %q", test.raw, test.secureMode, got, test.want)
			}
		})
	}
}

// testBase64Key is 32 zero bytes, base64-encoded: a validly-shaped (but not
// secret) value for SESSION_SIGNING_KEY / BACKUP_ENVELOPE_KEY entries.
const testBase64Key = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

// completeEnv sets every required variable to a valid value. Tests then
// unset or override one thing at a time.
func completeEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DB_DSN", "postgres://example.invalid/simplehost?sslmode=verify-full&sslrootcert=%2Fetc%2Fsimple-host%2Fdb-ca%2Fca.crt")
	t.Setenv("DB_HOST", "")
	t.Setenv("DB_USER", "")
	t.Setenv("DB_PASSWORD", "")
	t.Setenv("DB_NAME", "")
	t.Setenv("SECURE_MODE", "true")
	t.Setenv("PUBLIC_BASE_URL", "https://example.com/")
	t.Setenv("PORT", "8080")
	t.Setenv("HTTPS_REDIRECT_PORT", "8081")
	t.Setenv("OIDC_ISSUER", "https://issuer.example.com")
	t.Setenv("OIDC_CLIENT_ID", "test-client")
	t.Setenv("OIDC_CLIENT_SECRET", "test-secret")
	t.Setenv("SESSION_SIGNING_KEY", "k1:"+testBase64Key)
	t.Setenv("BACKUP_STORAGE_ENDPOINT", "https://s3.example.com")
	t.Setenv("BACKUP_STORAGE_REGION", "us-east-1")
	t.Setenv("BACKUP_STORAGE_BUCKET", "simple-host-backups")
	t.Setenv("BACKUP_STORAGE_ACCESS_KEY_ID", "")
	t.Setenv("BACKUP_STORAGE_SECRET_ACCESS_KEY", "")
	t.Setenv("BACKUP_STORAGE_INSECURE_ALLOWED", "")
	t.Setenv("BACKUP_SSE", "")
	t.Setenv("BACKUP_SSE_KEY_ID", "")
	t.Setenv("BACKUP_ENVELOPE_KEY", "")
	t.Setenv("DB_INSECURE_ALLOWED", "")
	t.Setenv("RESERVED_LABELS", "")
	t.Setenv("DB_APP_USER", "")
	t.Setenv("BACKUP_ENVELOPE_KEY_FILE", "")
	t.Setenv("TRUSTED_PROXY_CIDRS", "")
	t.Setenv("API_KEY_MAX_DAYS", "")
}

func TestLoadCompleteConfiguration(t *testing.T) {
	completeEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SecureMode {
		t.Fatal("SecureMode = false, want true")
	}
	if cfg.PublicBaseURL != "https://example.com" {
		t.Fatalf("PublicBaseURL = %q", cfg.PublicBaseURL)
	}
	if cfg.RedirectPort != "8081" {
		t.Fatalf("RedirectPort = %q", cfg.RedirectPort)
	}
	if cfg.Backup.Prefix != "backups/" {
		t.Fatalf("Backup.Prefix = %q, want the layout default", cfg.Backup.Prefix)
	}
	if cfg.CacheDir != defaultCacheDir || cfg.CacheMaxBytes != defaultCacheMaxBytes {
		t.Fatalf("CacheDir, CacheMaxBytes = %q, %d, want the defaults", cfg.CacheDir, cfg.CacheMaxBytes)
	}
	if cfg.Audit.RetentionDays != defaultAuditRetentionDays {
		t.Fatalf("Audit.RetentionDays = %d, want the default %d", cfg.Audit.RetentionDays, defaultAuditRetentionDays)
	}
	if cfg.Audit.AccessLogRetentionDays != defaultAccessLogRetentionDays {
		t.Fatalf("Audit.AccessLogRetentionDays = %d, want the default %d", cfg.Audit.AccessLogRetentionDays, defaultAccessLogRetentionDays)
	}
	if cfg.Audit.AccessLogVisibility != defaultAccessLogVisibility {
		t.Fatalf("Audit.AccessLogVisibility = %q, want the default %q", cfg.Audit.AccessLogVisibility, defaultAccessLogVisibility)
	}
}

// TestLoadAuditRetention covers design.md 8.2's three retention/visibility
// settings on their own, independent of Load's much larger required-value
// set — this is what cmd/server's `simple-host prune` subcommand calls, and
// it must succeed with none of Load's other environment variables set.
func TestLoadAuditRetention(t *testing.T) {
	t.Run("defaults when unset", func(t *testing.T) {
		t.Setenv("AUDIT_RETENTION_DAYS", "")
		t.Setenv("ACCESS_LOG_RETENTION_DAYS", "")
		t.Setenv("ACCESS_LOG_VISIBILITY", "")
		cfg, err := LoadAuditRetention()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.RetentionDays != defaultAuditRetentionDays || cfg.AccessLogRetentionDays != defaultAccessLogRetentionDays || cfg.AccessLogVisibility != defaultAccessLogVisibility {
			t.Fatalf("LoadAuditRetention() = %+v, want the documented defaults", cfg)
		}
	})

	t.Run("explicit values override the defaults", func(t *testing.T) {
		t.Setenv("AUDIT_RETENTION_DAYS", "30")
		t.Setenv("ACCESS_LOG_RETENTION_DAYS", "7")
		t.Setenv("ACCESS_LOG_VISIBILITY", "admin")
		cfg, err := LoadAuditRetention()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.RetentionDays != 30 || cfg.AccessLogRetentionDays != 7 || cfg.AccessLogVisibility != "admin" {
			t.Fatalf("LoadAuditRetention() = %+v, want {30 7 admin}", cfg)
		}
	})

	t.Run("rejects a visibility value that is neither owner nor admin", func(t *testing.T) {
		t.Setenv("AUDIT_RETENTION_DAYS", "")
		t.Setenv("ACCESS_LOG_RETENTION_DAYS", "")
		t.Setenv("ACCESS_LOG_VISIBILITY", "everyone")
		if _, err := LoadAuditRetention(); err == nil {
			t.Fatal("LoadAuditRetention() accepted an invalid ACCESS_LOG_VISIBILITY, want an error")
		}
	})

	for _, bad := range []string{"0", "-5", "not-a-number"} {
		t.Run("rejects a non-positive AUDIT_RETENTION_DAYS "+bad, func(t *testing.T) {
			t.Setenv("AUDIT_RETENTION_DAYS", bad)
			t.Setenv("ACCESS_LOG_RETENTION_DAYS", "")
			t.Setenv("ACCESS_LOG_VISIBILITY", "")
			if _, err := LoadAuditRetention(); err == nil {
				t.Fatalf("LoadAuditRetention() accepted AUDIT_RETENTION_DAYS=%q, want an error", bad)
			}
		})
	}
}

// Every value that identifies an installation is required, and the error names
// all of them at once so one restart shows the whole gap.
func TestLoadReportsEveryMissingValue(t *testing.T) {
	completeEnv(t)
	for _, key := range []string{"PUBLIC_BASE_URL", "OIDC_ISSUER", "BACKUP_STORAGE_ENDPOINT", "BACKUP_STORAGE_BUCKET", "DB_DSN"} {
		t.Setenv(key, "")
	}
	_, err := Load()
	if err == nil {
		t.Fatal("Load() succeeded with nothing configured")
	}
	for _, want := range []string{"PUBLIC_BASE_URL", "OIDC_ISSUER", "BACKUP_STORAGE_ENDPOINT", "BACKUP_STORAGE_BUCKET", "DB_DSN or DB_HOST"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Load() error %q does not name %s", err, want)
		}
	}
}

func TestLoadHasNoAdminAPIKey(t *testing.T) {
	// ADMIN_API_KEY and the synthetic admin it backed are gone (design.md
	// 6.2): admin status now follows OIDC claims, and Load must not require
	// or read the old variable at all.
	completeEnv(t)
	t.Setenv("ADMIN_API_KEY", "leftover-from-an-old-environment")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() with a stray ADMIN_API_KEY set: %v", err)
	}
	_ = cfg
}

func TestLoadRequiresOIDCConfiguration(t *testing.T) {
	for _, key := range []string{"OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET", "SESSION_SIGNING_KEY"} {
		t.Run(key, func(t *testing.T) {
			completeEnv(t)
			t.Setenv(key, "")
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("Load() error = %v, want %s reported missing", err, key)
			}
		})
	}
}

func TestLoadOIDCClaimMapping(t *testing.T) {
	completeEnv(t)
	t.Setenv("OIDC_SCOPES", "openid email profile groups")
	t.Setenv("OIDC_USERNAME_CLAIM", "preferred_username")
	t.Setenv("OIDC_ADMIN_CLAIM", "groups")
	t.Setenv("OIDC_ADMIN_VALUE", "simple-host-admins")
	t.Setenv("ADMIN_EMAILS", "Alice@Example.com, bob@example.com")
	t.Setenv("ALLOWED_EMAIL_DOMAINS", "example.com")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.OIDC.Scopes; len(got) != 4 || got[3] != "groups" {
		t.Fatalf("Scopes = %v", got)
	}
	if cfg.OIDC.UsernameClaim != "preferred_username" {
		t.Fatalf("UsernameClaim = %q", cfg.OIDC.UsernameClaim)
	}
	if cfg.OIDC.AdminClaim != "groups" || cfg.OIDC.AdminValue != "simple-host-admins" {
		t.Fatalf("admin claim/value = %q/%q", cfg.OIDC.AdminClaim, cfg.OIDC.AdminValue)
	}
	if want := []string{"alice@example.com", "bob@example.com"}; !equalStrings(cfg.OIDC.AdminEmails, want) {
		t.Fatalf("AdminEmails = %v, want %v (lowercased, trimmed)", cfg.OIDC.AdminEmails, want)
	}
	// Exactly one allowed domain: it becomes the default hint.
	if cfg.OIDC.HintDomain != "example.com" {
		t.Fatalf("HintDomain = %q, want the sole allowed domain as default", cfg.OIDC.HintDomain)
	}
}

func TestLoadRejectsUnpairedAdminClaim(t *testing.T) {
	completeEnv(t)
	t.Setenv("OIDC_ADMIN_CLAIM", "groups")
	t.Setenv("OIDC_ADMIN_VALUE", "")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "OIDC_ADMIN_CLAIM") {
		t.Fatalf("Load() error = %v, want the unpaired admin claim refused", err)
	}
}

func TestLoadDefaultsSessionLifetimes(t *testing.T) {
	completeEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Session.TTL != defaultSessionTTL {
		t.Fatalf("Session.TTL = %v, want default %v", cfg.Session.TTL, defaultSessionTTL)
	}
	if cfg.Session.Idle != defaultSessionIdle {
		t.Fatalf("Session.Idle = %v, want default %v", cfg.Session.Idle, defaultSessionIdle)
	}
	if len(cfg.Session.SigningKeys) != 1 || cfg.Session.SigningKeys[0].ID != "k1" {
		t.Fatalf("Session.SigningKeys = %+v", cfg.Session.SigningKeys)
	}
}

func TestLoadParsesSessionLifetimeOverrides(t *testing.T) {
	completeEnv(t)
	t.Setenv("SESSION_TTL", "2h")
	t.Setenv("SESSION_IDLE", "15m")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Session.TTL.String() != "2h0m0s" || cfg.Session.Idle.String() != "15m0s" {
		t.Fatalf("Session = %+v", cfg.Session)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDatabaseDSNFromParts(t *testing.T) {
	completeEnv(t)
	t.Setenv("DB_DSN", "")
	t.Setenv("DB_HOST", "postgres.simple-host.svc")
	t.Setenv("DB_USER", "app")
	t.Setenv("DB_PASSWORD", "p@ss word")
	t.Setenv("DB_NAME", "simplehost")
	t.Setenv("DB_SSL_ROOT_CERT", "/etc/simple-host/db-ca/ca.crt")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := "postgres://app:p%40ss%20word@postgres.simple-host.svc:5432/simplehost?sslmode=verify-full&sslrootcert=%2Fetc%2Fsimple-host%2Fdb-ca%2Fca.crt"
	if cfg.DBDSN != want {
		t.Fatalf("DBDSN = %q, want %q", cfg.DBDSN, want)
	}

	t.Setenv("DB_PASSWORD", "")
	_, err = Load()
	if err == nil || !strings.Contains(err.Error(), "DB_PASSWORD") {
		t.Fatalf("Load() error = %v, want the missing part named", err)
	}
}

// TestDatabaseDSNRefusesKeywordValueForm is the first half of the Phase 5
// review finding folded into Phase 1: lib/pq accepts a keyword/value DSN
// ("host=... sslmode=..." — no "://" anywhere) as well as a URL, but
// validateDatabaseSSL used to run url.Parse on whatever DB_DSN held
// regardless of shape. A keyword/value string has no query component for
// url.Parse to find, so sslmode read back as empty and a perfectly correct
// DSN was refused. It must now be refused for a different, explicit reason:
// this form isn't accepted at all.
func TestDatabaseDSNRefusesKeywordValueForm(t *testing.T) {
	completeEnv(t)
	t.Setenv("DB_DSN", "host=postgres.internal port=5432 user=app password=secret dbname=simplehost sslmode=verify-full sslrootcert=/etc/simple-host/db-ca/ca.crt")
	_, err := Load()
	if err == nil {
		t.Fatal("Load() accepted a keyword/value DB_DSN, want it refused")
	}
	if !strings.Contains(err.Error(), "DB_DSN") || !strings.Contains(err.Error(), "keyword/value DSN") {
		t.Fatalf("Load() error = %v, want DB_DSN named and the keyword/value shape called out", err)
	}
}

// TestDatabaseDSNRefusesQuotedPasswordQueryStringBypass is the second, worse
// half of the same finding: a keyword/value DSN can be crafted so that
// url.Parse's naive first-"?" split hands validateDatabaseSSL a fake
// "sslmode=verify-full" it reads out of the middle of a quoted field —
// here, the password — while the DSN's own real, explicit "sslmode=disable"
// keyword is what lib/pq actually connects with. The fix must refuse this
// outright rather than merely fail to be fooled by it, since refusing every
// non-URL shape makes the specific trick irrelevant.
func TestDatabaseDSNRefusesQuotedPasswordQueryStringBypass(t *testing.T) {
	completeEnv(t)
	t.Setenv("DB_DSN",
		`host=postgres.internal port=5432 user=app password='innocuous?sslmode=verify-full&sslrootcert=/etc/simple-host/db-ca/ca.crt' dbname=simplehost sslmode=disable`)
	_, err := Load()
	if err == nil {
		t.Fatal("Load() accepted a keyword/value DB_DSN carrying a query-string-shaped password, want it refused")
	}
	if !strings.Contains(err.Error(), "DB_DSN") {
		t.Fatalf("Load() error = %v, want DB_DSN named", err)
	}
}

// TestDatabaseDSNAcceptsAndNormalizesURLForm is the accept-path half: a
// genuine postgres:// URL with sslmode=verify-full and sslrootcert set is
// accepted, and the DSN Load() records is the re-serialized form
// normalizeDatabaseDSNURL produces — the same string validateDatabaseSSL
// checked, byte for byte, is the one a caller would open a connection with.
func TestDatabaseDSNAcceptsAndNormalizesURLForm(t *testing.T) {
	completeEnv(t)
	t.Setenv("DB_DSN", "postgresql://app:p%40ss@postgres.internal:5432/simplehost?sslmode=verify-full&sslrootcert=%2Fetc%2Fsimple-host%2Fdb-ca%2Fca.crt")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() rejected a well-formed URL DSN: %v", err)
	}
	parsed, err := url.Parse(cfg.DBDSN)
	if err != nil {
		t.Fatalf("Load() produced an unparseable DBDSN %q: %v", cfg.DBDSN, err)
	}
	if got := strings.ToLower(parsed.Scheme); got != "postgresql" {
		t.Fatalf("DBDSN scheme = %q, want the original postgresql scheme preserved", got)
	}
	if got := parsed.Query().Get("sslmode"); !strings.EqualFold(got, "verify-full") {
		t.Fatalf("DBDSN sslmode = %q, want verify-full", got)
	}
	if got := parsed.Query().Get("sslrootcert"); got == "" {
		t.Fatal("DBDSN lost sslrootcert in normalization")
	}
}

func TestLoadRejectsInvalidSecurityConfiguration(t *testing.T) {
	t.Run("invalid secure mode", func(t *testing.T) {
		completeEnv(t)
		t.Setenv("SECURE_MODE", "sometimes")
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "SECURE_MODE") {
			t.Fatalf("Load() error = %v, want SECURE_MODE validation error", err)
		}
	})

	t.Run("same listener port", func(t *testing.T) {
		completeEnv(t)
		t.Setenv("HTTPS_REDIRECT_PORT", "8080")
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "must differ") {
			t.Fatalf("Load() error = %v, want listener-port validation error", err)
		}
	})

	t.Run("metrics port shares a listener", func(t *testing.T) {
		for _, port := range []string{"8080", "8081"} {
			completeEnv(t)
			t.Setenv("METRICS_PORT", port)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "METRICS_PORT") {
				t.Fatalf("METRICS_PORT=%s: Load() error = %v, want it refused", port, err)
			}
		}
	})

	t.Run("plain http bucket endpoint", func(t *testing.T) {
		completeEnv(t)
		t.Setenv("BACKUP_STORAGE_ENDPOINT", "http://minio.simple-host.svc:9000")
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "BACKUP_STORAGE_ENDPOINT") {
			t.Fatalf("Load() error = %v, want the http endpoint refused", err)
		}
		t.Setenv("BACKUP_STORAGE_INSECURE_ALLOWED", "true")
		if _, err := Load(); err != nil {
			t.Fatalf("Load() with the local override: %v", err)
		}
	})

	t.Run("half a key pair", func(t *testing.T) {
		completeEnv(t)
		t.Setenv("BACKUP_STORAGE_ACCESS_KEY_ID", "minio")
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "set together") {
			t.Fatalf("Load() error = %v, want the key pair validated as a pair", err)
		}
	})
}

func TestLoadParsesReservedLabels(t *testing.T) {
	t.Run("default empty", func(t *testing.T) {
		completeEnv(t)
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.ReservedLabels) != 0 {
			t.Fatalf("ReservedLabels = %v, want empty by default", cfg.ReservedLabels)
		}
	})

	t.Run("splits, lowercases and trims a comma list", func(t *testing.T) {
		completeEnv(t)
		t.Setenv("RESERVED_LABELS", " Acme , Corp-Internal ,acme")
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"acme", "corp-internal", "acme"}
		if len(cfg.ReservedLabels) != len(want) {
			t.Fatalf("ReservedLabels = %v, want %v", cfg.ReservedLabels, want)
		}
		for i, label := range want {
			if cfg.ReservedLabels[i] != label {
				t.Fatalf("ReservedLabels[%d] = %q, want %q", i, cfg.ReservedLabels[i], label)
			}
		}
	})
}

// TestLoadRefusesWeakDatabaseTLS is design 9.2's one line: verify-full with a
// root cert, or the process does not start, unless the local-evaluation
// override is set.
func TestLoadRefusesWeakDatabaseTLS(t *testing.T) {
	t.Run("sslmode weaker than verify-full is refused", func(t *testing.T) {
		completeEnv(t)
		t.Setenv("DB_DSN", "")
		t.Setenv("DB_HOST", "postgres.simple-host.svc")
		t.Setenv("DB_USER", "app")
		t.Setenv("DB_PASSWORD", "secret")
		t.Setenv("DB_NAME", "simplehost")
		t.Setenv("DB_SSLMODE", "require")
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "verify-full") {
			t.Fatalf("Load() error = %v, want sslmode=require refused", err)
		}
	})

	t.Run("verify-full with no sslrootcert is refused", func(t *testing.T) {
		completeEnv(t)
		t.Setenv("DB_DSN", "")
		t.Setenv("DB_HOST", "postgres.simple-host.svc")
		t.Setenv("DB_USER", "app")
		t.Setenv("DB_PASSWORD", "secret")
		t.Setenv("DB_NAME", "simplehost")
		t.Setenv("DB_SSL_ROOT_CERT", "")
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "sslrootcert") {
			t.Fatalf("Load() error = %v, want missing sslrootcert refused", err)
		}
	})

	t.Run("a DSN passed whole is checked too", func(t *testing.T) {
		completeEnv(t)
		t.Setenv("DB_DSN", "postgres://example.invalid/simplehost?sslmode=disable")
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "verify-full") {
			t.Fatalf("Load() error = %v, want sslmode=disable in a raw DSN refused", err)
		}
	})

	t.Run("the local-evaluation override permits both", func(t *testing.T) {
		completeEnv(t)
		t.Setenv("DB_DSN", "postgres://example.invalid/simplehost?sslmode=disable")
		t.Setenv("DB_INSECURE_ALLOWED", "true")
		if _, err := Load(); err != nil {
			t.Fatalf("Load() with DB_INSECURE_ALLOWED=true: %v", err)
		}
	})

	t.Run("LoadDatabase applies the same refusal", func(t *testing.T) {
		completeEnv(t)
		t.Setenv("DB_DSN", "postgres://example.invalid/simplehost?sslmode=disable")
		_, err := LoadDatabase()
		if err == nil || !strings.Contains(err.Error(), "verify-full") {
			t.Fatalf("LoadDatabase() error = %v, want sslmode=disable refused", err)
		}
		t.Setenv("DB_INSECURE_ALLOWED", "true")
		if _, err := LoadDatabase(); err != nil {
			t.Fatalf("LoadDatabase() with the override: %v", err)
		}
	})
}

func TestLoadValidatesBackupSSE(t *testing.T) {
	t.Run("defaults to AES256", func(t *testing.T) {
		completeEnv(t)
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Backup.SSE != "AES256" {
			t.Fatalf("Backup.SSE = %q, want AES256 by default", cfg.Backup.SSE)
		}
	})

	t.Run("aws:kms requires a key id", func(t *testing.T) {
		completeEnv(t)
		t.Setenv("BACKUP_SSE", "aws:kms")
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "BACKUP_SSE_KEY_ID") {
			t.Fatalf("Load() error = %v, want aws:kms with no key id refused", err)
		}
		t.Setenv("BACKUP_SSE_KEY_ID", "arn:aws:kms:us-east-1:111111111111:key/abc")
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Backup.SSE != "aws:kms" || cfg.Backup.SSEKMSKeyID == "" {
			t.Fatalf("Backup.SSE = %q, SSEKMSKeyID = %q", cfg.Backup.SSE, cfg.Backup.SSEKMSKeyID)
		}
	})

	t.Run("a key id without aws:kms is refused", func(t *testing.T) {
		completeEnv(t)
		t.Setenv("BACKUP_SSE_KEY_ID", "arn:aws:kms:us-east-1:111111111111:key/abc")
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "BACKUP_SSE_KEY_ID") {
			t.Fatalf("Load() error = %v, want the stray key id refused", err)
		}
	})

	t.Run("an unrecognised mode is refused", func(t *testing.T) {
		completeEnv(t)
		t.Setenv("BACKUP_SSE", "SSE-C")
		_, err := Load()
		if err == nil {
			t.Fatal("Load() accepted an unrecognised BACKUP_SSE value")
		}
	})
}

func TestParseEnvelopeKeys(t *testing.T) {
	const key1 = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" // 32 zero bytes, base64
	const key2 = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=" // 32 0x01 bytes, base64

	t.Run("empty disables the envelope", func(t *testing.T) {
		keys, err := parseEnvelopeKeys("")
		if err != nil || keys != nil {
			t.Fatalf("parseEnvelopeKeys(\"\") = %v, %v, want nil, nil", keys, err)
		}
	})

	t.Run("one key", func(t *testing.T) {
		keys, err := parseEnvelopeKeys("k1:" + key1)
		if err != nil {
			t.Fatal(err)
		}
		if len(keys) != 1 || keys[0].ID != "k1" || len(keys[0].Key) != 32 {
			t.Fatalf("parseEnvelopeKeys = %+v", keys)
		}
	})

	t.Run("two keys, order preserved for rotation", func(t *testing.T) {
		keys, err := parseEnvelopeKeys("k1:" + key1 + ",k2:" + key2)
		if err != nil {
			t.Fatal(err)
		}
		if len(keys) != 2 || keys[0].ID != "k1" || keys[1].ID != "k2" {
			t.Fatalf("parseEnvelopeKeys = %+v, want k1 first", keys)
		}
	})

	t.Run("more than eight keys is refused", func(t *testing.T) {
		raw := "k1:" + key1
		for i := 2; i <= 9; i++ {
			raw += fmt.Sprintf(",k%d:%s", i, key2)
		}
		if _, err := parseEnvelopeKeys(raw); err == nil {
			t.Fatal("parseEnvelopeKeys accepted nine keys")
		}
	})

	t.Run("duplicate id is refused", func(t *testing.T) {
		if _, err := parseEnvelopeKeys("k1:" + key1 + ",k1:" + key2); err == nil {
			t.Fatal("parseEnvelopeKeys accepted a repeated id")
		}
	})

	t.Run("wrong length is refused", func(t *testing.T) {
		if _, err := parseEnvelopeKeys("k1:AAAA"); err == nil {
			t.Fatal("parseEnvelopeKeys accepted a short key")
		}
	})

	t.Run("malformed entry is refused", func(t *testing.T) {
		if _, err := parseEnvelopeKeys("not-a-key-value-pair"); err == nil {
			t.Fatal("parseEnvelopeKeys accepted an entry with no id")
		}
	})

	t.Run("Load wires it through", func(t *testing.T) {
		completeEnv(t)
		t.Setenv("BACKUP_ENVELOPE_KEY", "k1:"+key1)
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Backup.EnvelopeKeys) != 1 || cfg.Backup.EnvelopeKeys[0].ID != "k1" {
			t.Fatalf("cfg.Backup.EnvelopeKeys = %+v", cfg.Backup.EnvelopeKeys)
		}
	})
}

func TestBackupRegionDefaultsBecauseItIsAnAWSConcept(t *testing.T) {
	// Region only means something on AWS. Every other S3-compatible store
	// derives it from the endpoint and accepts any value, so requiring it made
	// each non-AWS installation type a magic word to get past startup.
	completeEnv(t)
	t.Setenv("BACKUP_STORAGE_REGION", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with no region: %v", err)
	}
	if cfg.Backup.Region != defaultBackupRegion {
		t.Errorf("Region = %q, want the %q default", cfg.Backup.Region, defaultBackupRegion)
	}

	// And a provider that does care is still obeyed.
	t.Setenv("BACKUP_STORAGE_REGION", "eu-west-2")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load with a region: %v", err)
	}
	if cfg.Backup.Region != "eu-west-2" {
		t.Errorf("Region = %q, want eu-west-2", cfg.Backup.Region)
	}
}

func TestSecretsCanArriveAsFilesRatherThanEnvironment(t *testing.T) {
	// The cloud-neutral path. Every cloud's workload identity is different, but
	// every cloud's secret store has a Kubernetes-native projection that lands
	// a file in the container — the Secrets Store CSI driver, or External
	// Secrets Operator writing a Secret. A file also keeps the value out of the
	// environment, where every child process and every crash dump can see it.
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	completeEnv(t)
	t.Setenv("OIDC_CLIENT_SECRET", "")
	t.Setenv("OIDC_CLIENT_SECRET_FILE", write("oidc", "from-a-file"))
	t.Setenv("BACKUP_STORAGE_ACCESS_KEY_ID", "")
	t.Setenv("BACKUP_STORAGE_ACCESS_KEY_ID_FILE", write("akid", "AKIAEXAMPLE"))
	t.Setenv("BACKUP_STORAGE_SECRET_ACCESS_KEY", "")
	t.Setenv("BACKUP_STORAGE_SECRET_ACCESS_KEY_FILE", write("secret", "s3cr3t"))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with file-backed secrets: %v", err)
	}
	if cfg.OIDC.ClientSecret != "from-a-file" {
		t.Errorf("ClientSecret = %q", cfg.OIDC.ClientSecret)
	}
	// Trailing newline trimmed: every tool that writes a secret file adds one,
	// and a credential with \n on the end fails authentication in a way that
	// gives no hint why.
	if cfg.Backup.AccessKeyID != "AKIAEXAMPLE" || cfg.Backup.SecretAccessKey != "s3cr3t" {
		t.Errorf("storage credentials = %q / %q", cfg.Backup.AccessKeyID, cfg.Backup.SecretAccessKey)
	}
}

func TestAnUnreadableSecretFileIsNotTreatedAsUnset(t *testing.T) {
	// A broken mount and an unset variable need different fixes. Reporting the
	// first as the second sends the operator to set a value that is already set.
	completeEnv(t)
	t.Setenv("OIDC_CLIENT_SECRET", "")
	t.Setenv("OIDC_CLIENT_SECRET_FILE", filepath.Join(t.TempDir(), "never-mounted"))

	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted a secret file that does not exist")
	}
	if !strings.Contains(err.Error(), "OIDC_CLIENT_SECRET_FILE") {
		t.Errorf("error does not name the file variable: %v", err)
	}
	if strings.Contains(err.Error(), "missing required configuration") {
		t.Errorf("an unreadable mount was reported as a missing value: %v", err)
	}
}

func TestLoadRefusesMultiTenantEntraIssuer(t *testing.T) {
	for _, issuer := range []string{
		"https://login.microsoftonline.com/common/v2.0",
		"https://login.microsoftonline.com/organizations/v2.0",
		"https://login.microsoftonline.com/consumers/v2.0",
	} {
		completeEnv(t)
		t.Setenv("OIDC_ISSUER", issuer)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "OIDC_ISSUER") {
			t.Errorf("Load accepted %s: %v", issuer, err)
		}
	}
	completeEnv(t)
	t.Setenv("OIDC_ISSUER", "https://login.microsoftonline.com/0f1e2d3c-0000-0000-0000-000000000000/v2.0")
	if _, err := Load(); err != nil {
		t.Fatalf("tenant-specific Entra issuer: %v", err)
	}
}

func TestAnUnreadableEnvelopeKeyFileStopsStartup(t *testing.T) {
	completeEnv(t)
	t.Setenv("BACKUP_ENVELOPE_KEY_FILE", filepath.Join(t.TempDir(), "never-mounted"))
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "BACKUP_ENVELOPE_KEY_FILE") {
		t.Fatalf("Load with an unreadable envelope key file: %v", err)
	}
}

func partsEnv(t *testing.T) {
	t.Helper()
	completeEnv(t)
	t.Setenv("DB_DSN", "")
	t.Setenv("DB_HOST", "db.internal")
	t.Setenv("DB_USER", "simplehost")
	t.Setenv("DB_PASSWORD", "owner-secret")
	t.Setenv("DB_NAME", "simplehost")
	t.Setenv("DB_SSL_ROOT_CERT", "/etc/ca.crt")
}

func TestServerConnectsAsTheAppRole(t *testing.T) {
	partsEnv(t)
	t.Setenv("DB_APP_USER", "simplehost_app")
	t.Setenv("DB_APP_PASSWORD", "")
	// A password file for the owning role must not decide the server's
	// credential (it used to win over the manifest's DB_PASSWORD override).
	ownerFile := filepath.Join(t.TempDir(), "owner")
	if err := os.WriteFile(ownerFile, []byte("owner-from-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DB_PASSWORD_FILE", ownerFile)
	appFile := filepath.Join(t.TempDir(), "app")
	if err := os.WriteFile(appFile, []byte("app-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DB_APP_PASSWORD_FILE", appFile)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(cfg.DBDSN)
	if pw, _ := u.User.Password(); u.User.Username() != "simplehost_app" || pw != "app-from-file" {
		t.Fatalf("server DSN user = %q, password from the wrong source", u.User.Username())
	}

	t.Setenv("DB_APP_PASSWORD_FILE", "")
	t.Setenv("DB_PASSWORD_FILE", "")
	t.Setenv("DB_APP_PASSWORD", "owner-secret")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("shared password accepted: %v", err)
	}
	if _, err := LoadAppRolePassword(); err == nil {
		t.Fatal("LoadAppRolePassword accepted a password equal to DB_PASSWORD")
	}

	t.Setenv("DB_APP_PASSWORD", "app-secret")
	t.Setenv("DB_DSN", "postgres://x@db.internal/simplehost?sslmode=verify-full&sslrootcert=%2Fetc%2Fca.crt")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "DB_APP_USER") {
		t.Fatalf("DB_APP_USER with DB_DSN accepted: %v", err)
	}
}

func TestDSNParseErrorDoesNotLeakThePassword(t *testing.T) {
	completeEnv(t)
	t.Setenv("DB_DSN", "postgres://user:hunter2%zz@db/x")
	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted an unparseable DSN")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error leaks the password: %v", err)
	}
}

func TestTrustedProxiesAndAPIKeyMaxDays(t *testing.T) {
	completeEnv(t)
	t.Setenv("TRUSTED_PROXY_CIDRS", "10.0.0.0/8, 192.168.1.7 ,fd00::/8")
	t.Setenv("API_KEY_MAX_DAYS", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TrustedProxies) != 3 || cfg.TrustedProxies[1].String() != "192.168.1.7/32" {
		t.Fatalf("TrustedProxies = %v", cfg.TrustedProxies)
	}
	if cfg.APIKeyMaxDays != 365 {
		t.Fatalf("APIKeyMaxDays = %d", cfg.APIKeyMaxDays)
	}
	t.Setenv("TRUSTED_PROXY_CIDRS", "not-a-cidr")
	if _, err := Load(); err == nil {
		t.Fatal("bad TRUSTED_PROXY_CIDRS accepted")
	}
	t.Setenv("TRUSTED_PROXY_CIDRS", "")
	for _, bad := range []string{"0", "366"} {
		t.Setenv("API_KEY_MAX_DAYS", bad)
		if _, err := Load(); err == nil {
			t.Fatalf("API_KEY_MAX_DAYS=%s accepted", bad)
		}
	}
}
