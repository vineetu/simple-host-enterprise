package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Defaults that name a port, a path or a layout, never an environment. Anything
// that identifies a particular installation (its public address, its database,
// its bucket, its admin credential) has no default and must be set, so a
// misconfigured pod fails at startup with the list of what is missing rather
// than quietly talking to somebody else's infrastructure.
const (
	defaultCacheDir      = "/var/cache/simple-host"
	defaultCacheMaxBytes = 1 << 30
	defaultPort          = "8080"
	defaultRedirectPort  = "8081"
	defaultMetricsPort   = "9090"
	defaultDBPort        = "5432"
	defaultDBSSLMode     = "verify-full"
	defaultBackupPrefix  = "backups/"
	// defaultBackupRegion is a placeholder, not a location. Region is an AWS
	// concept; most S3-compatible stores derive it from the endpoint and accept
	// any value, so requiring one only made every installation outside AWS type
	// a magic word. Set it when your provider cares.
	defaultBackupRegion = "us-east-1"
	defaultBackupSSE    = "AES256"

	sseAES256 = "AES256"
	sseKMS    = "aws:kms"

	// envelopeKeyLength is the fixed size of a BACKUP_ENVELOPE_KEY entry's
	// key material: 32 random bytes, used directly as an AES-256 key.
	envelopeKeyLength = 32
	// maxEnvelopeKeys: the first key wraps, every configured key unwraps.
	// A retired key stays configured until `simple-host reencrypt` has
	// rewritten every object under the first key; the limit leaves room for
	// rotations whose old keys have not been removed yet.
	maxEnvelopeKeys = 8

	// signingKeyLength is the fixed size of a SESSION_SIGNING_KEY entry: 32
	// random bytes, used directly as an HMAC-SHA256 key.
	signingKeyLength = 32
	// maxSigningKeys is the rotation shape: the first
	// key signs every new cookie, and every configured key is tried to
	// verify one already out in a browser.
	maxSigningKeys = 2

	defaultOIDCScopes     = "openid email profile"
	defaultOIDCEmailClaim = "email"
	// Session lifetimes. Simple Host checks the identity provider only at
	// sign-in, so these bound how long someone disabled at the IdP can keep
	// working here. The absolute default is a working day, 8 hours, which
	// is also where common IdPs set their own session default; the idle
	// default of 30 minutes ends an unattended browser's session well
	// before that. The caps stop a typo (24000h, 8d) from making a session
	// effectively permanent.
	defaultSessionTTL  = 8 * time.Hour
	defaultSessionIdle = 30 * time.Minute
	maxSessionTTL      = 24 * time.Hour
	maxSessionIdle     = 8 * time.Hour

	// OAuth connector tokens (an AI app connected to /mcp). The access
	// token is short, so revoking the app or disabling the person takes
	// effect within the hour even for a copy held elsewhere; the refresh
	// token's lifetime runs from the grant's sign-in, not from its last
	// rotation, so the person signs in at the IdP again at least this
	// often (handler/connector.go).
	defaultOAuthAccessTTL  = time.Hour
	maxOAuthAccessTTL      = 24 * time.Hour
	defaultOAuthRefreshTTL = 30 * 24 * time.Hour
	maxOAuthRefreshTTL     = 90 * 24 * time.Hour

	// Asset upload limits: a per-file size cap, a per-site
	// total-bytes cap, and a per-site count cap. All three are overridable —
	// an installation with more storage to spare, or a plan tier, may want
	// higher ceilings than these defaults.
	defaultAssetMaxFileBytes = 25 << 20  // 25 MiB
	defaultAssetMaxSiteBytes = 500 << 20 // 500 MiB
	defaultAssetMaxSiteCount = 5000

	// Audit/access-log retention defaults.
	defaultAuditRetentionDays     = 400
	defaultAccessLogRetentionDays = 90
	defaultAccessLogVisibility    = "counts"

	// maxAPIKeyDays caps API_KEY_MAX_DAYS: an API key is for CI and never
	// lives longer than a year.
	maxAPIKeyDays = 365
)

type Config struct {
	DBDSN string
	// CacheDir is the pod-local directory the site store caches versions
	// and assets in; CacheMaxBytes bounds it (internal/storage cache.go).
	CacheDir      string
	CacheMaxBytes int64
	Port          string
	RedirectPort  string
	// MetricsPort serves /metrics on its own listener, which the Service and
	// Ingress never expose.
	MetricsPort   string
	PublicBaseURL string
	// DBInClusterEvaluation is set by deploy/components/postgres-incluster:
	// a single unbacked-up Postgres fit only for evaluation.
	DBInClusterEvaluation bool
	OIDC                  OIDCConfig
	Session               SessionConfig
	// ReservedLabels extends the built-in set in names.go with
	// installation-specific hostnames that must never belong to an account,
	// on top of the built-in set.
	ReservedLabels []string
	SecureMode     bool
	Backup         BackupConfig
	// Assets bounds an upload through the site-facing API:
	// per-file size, per-site total bytes, and per-site count.
	Assets AssetLimits
	// TrustedProxies is TRUSTED_PROXY_CIDRS: peers whose X-Forwarded-For
	// is believed when working out a request's client address. Empty means
	// the TCP peer is the client.
	TrustedProxies []netip.Prefix
	// APIKeyMaxDays is API_KEY_MAX_DAYS: the longest lifetime a new API key
	// may be minted with (1 to 365, default 365).
	APIKeyMaxDays int64
	// Audit is the retention and visibility knobs for audit_events and
	// access_log. cmd/server's prune subcommand read these two directly
	// from the environment ahead of this field existing (see
	// docs/security-review.md); both now go through this package instead,
	// the same place every other environment-derived setting lives.
	Audit AuditConfig
	// OAuthRedirectHosts is where an AI app connecting to /mcp may be sent
	// back after sign-in (OAUTH_REDIRECT_HOSTS): hostnames for https
	// redirects, "localhost" for loopback redirects of apps on the person's
	// own machine, "scheme://host" for an app's own URL scheme, or "*" for
	// any https host.
	OAuthRedirectHosts []string
	// OAuthAccessTTL and OAuthRefreshTTL are OAUTH_ACCESS_TTL and
	// OAUTH_REFRESH_TTL: an AI app's access token lifetime, and how long a
	// connection lasts from sign-in before the person must sign in again.
	OAuthAccessTTL  time.Duration
	OAuthRefreshTTL time.Duration
	// NetworkAccessApprovals is NETWORK_ACCESS_APPROVALS: how many different
	// admins must approve a request to open a site to the network, 1 (the
	// default) or 2. The requester never counts.
	NetworkAccessApprovals int
}

// defaultOAuthRedirectHosts covers the AI apps a company is most likely to
// connect: ChatGPT, Claude, VS Code (Copilot), Cursor, and command-line
// agents on the person's own machine (Claude Code, Codex).
const defaultOAuthRedirectHosts = "chatgpt.com,claude.ai,vscode.dev,localhost,cursor://anysphere.cursor-mcp"

// AuditConfig holds the retention and visibility settings for
// audit_events and access_log.
type AuditConfig struct {
	// RetentionDays and AccessLogRetentionDays bound how long
	// audit_events and access_log rows are kept before `simple-host
	// prune` drops their partition.
	RetentionDays          int64
	AccessLogRetentionDays int64
	// AccessLogVisibility is "counts" (the default: an owner and their team
	// see views per day and how many distinct people viewed, never who),
	// "owner" (they also see each visit and who made it; IP and user agent
	// stay admin-only), or "admin" (only an admin reads access_log at all).
	AccessLogVisibility string
}

// AssetLimits mirrors storage.AssetLimits (this package does not import
// storage — the same one-struct-per-boundary shape BackupConfig's
// EnvelopeKey and SessionConfig's SigningKey already use) so cmd/server can
// copy it across the package boundary the same way it does for those.
type AssetLimits struct {
	MaxFileBytes int64
	MaxSiteBytes int64
	MaxSiteCount int64
}

// BackupConfig names the S3-compatible store that receives a copy of every
// uploaded version. Any store that speaks the S3 API will do; the endpoint is
// explicit so nothing here assumes one cloud.
type BackupConfig struct {
	Endpoint string
	Region   string
	Bucket   string
	Prefix   string
	// AccessKeyID and SecretAccessKey are optional: left empty, the SDK's
	// default chain is used, which is how a platform's workload identity
	// injects credentials without a key pair.
	AccessKeyID     string
	SecretAccessKey string
	// InsecureAllowed permits a plain-http endpoint. It exists for the local
	// cluster, where the bucket sits on the same node; a production endpoint
	// is https or the process refuses to start.
	InsecureAllowed bool
	// SSE is the x-amz-server-side-encryption header sent on every PutObject:
	// "AES256" (default, every S3-compatible store honours it) or "aws:kms".
	SSE string
	// SSEKMSKeyID is required when SSE is "aws:kms" and rejected otherwise.
	SSEKMSKeyID string
	// EnvelopePlaintextAllowed (BACKUP_ENVELOPE_PLAINTEXT_ALLOWED) lets an
	// install that added BACKUP_ENVELOPE_KEY after it already held objects
	// keep reading the ones written before; otherwise a plain object is
	// refused whenever the envelope is on.
	EnvelopePlaintextAllowed bool
	// EnvelopeKeys is optional client-side envelope encryption.
	// Empty means backups travel with only the SSE header above. 1 or 2 keys
	// mirror the SESSION_SIGNING_KEY rotation shape: the first wraps every
	// new object, and every key here is tried to unwrap an existing one.
	EnvelopeKeys []EnvelopeKey
}

// EnvelopeKey is one named 32-byte key for the optional client-side backup
// envelope. Config-level duplicate of storage.EnvelopeKey, for the reason
// BackupConfig above already gives: this package does not import storage.
type EnvelopeKey struct {
	ID  string
	Key []byte
}

// SigningKey is one named 32-byte HMAC key for signing and verifying the
// session cookie. Shares the rotation shape EnvelopeKey
// uses: the first entry signs every new cookie, every entry is tried, by
// id, to verify one already out in a browser.
type SigningKey struct {
	ID  string
	Key []byte
}

// OIDCConfig is the provider and claim-mapping configuration for sign-in.
// Nothing here is a secret except ClientSecret.
type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	// Scopes is space-separated in OIDC_SCOPES; parsed into a slice for
	// internal/oidc.Config.
	Scopes []string
	// EmailClaim names the claim this application reads as the person's
	// address. "email" for every provider this package is proven against;
	// configurable because OpenID Connect does not mandate the name.
	EmailClaim string
	// UsernameClaim, when set, names a claim used to derive the account's
	// username instead of the email's local part.
	UsernameClaim string
	// AdminClaim/AdminValue are an optional second path to admin, alongside
	// AdminEmails: a person is admin if either source grants it.
	AdminClaim string
	AdminValue string
	// AdminEmails is lowercased and trimmed; the portable admin path every
	// reference install documents, since neither Google nor Entra's common
	// endpoint puts groups in the ID token.
	AdminEmails []string
	// AllowedEmailDomains, lowercased: sign-in from any other domain is
	// refused. Empty means no restriction, which is only sound for a
	// single-tenant provider that already scopes who may authenticate.
	AllowedEmailDomains []string
	// HintDomain is sent as the provider's domain hint (Google: hd) on the
	// authorization request. Defaults to the sole entry of
	// AllowedEmailDomains when there is exactly one.
	HintDomain string
	// InsecureAllowed (OIDC_INSECURE_ALLOWED) permits a plain-http issuer
	// and discovered endpoints. For the local overlay's in-cluster Dex only.
	InsecureAllowed bool
}

// SessionConfig is the cookie and session-lifetime configuration.
type SessionConfig struct {
	SigningKeys []SigningKey
	TTL         time.Duration
	Idle        time.Duration
}

// missing collects the names of required variables that were not set, so the
// operator sees every gap in one message instead of one per restart.
type missing []string

func (m *missing) require(key string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		*m = append(*m, key)
	}
	return value
}

// requireSecret is require for a value that may arrive through KEY_FILE. A
// file that cannot be read is reported as its own error rather than counted as
// missing: "you did not set it" and "your secret mount is broken" need
// different fixes, and conflating them sends the operator to the wrong one.
func (m *missing) requireSecret(key string, fail *error) string {
	value, err := secretEnv(key)
	if err != nil {
		if *fail == nil {
			*fail = err
		}
		return ""
	}
	value = strings.TrimSpace(value)
	if value == "" {
		*m = append(*m, key)
	}
	return value
}

func (m missing) err() error {
	if len(m) == 0 {
		return nil
	}
	return fmt.Errorf("missing required configuration: %s", strings.Join(m, ", "))
}

func Load() (Config, error) {
	// Set when a KEY_FILE mount exists but cannot be read. Checked once, after
	// the required-value sweep, so one bad mount does not hide the rest.
	var secretErr error
	secureMode, err := boolEnv("SECURE_MODE", false)
	if err != nil {
		return Config{}, err
	}
	backupInsecure, err := boolEnv("BACKUP_STORAGE_INSECURE_ALLOWED", false)
	if err != nil {
		return Config{}, err
	}
	dbInsecureAllowed, err := boolEnv("DB_INSECURE_ALLOWED", false)
	if err != nil {
		return Config{}, err
	}
	oidcInsecureAllowed, err := boolEnv("OIDC_INSECURE_ALLOWED", false)
	if err != nil {
		return Config{}, err
	}
	envelopePlaintextAllowed, err := boolEnv("BACKUP_ENVELOPE_PLAINTEXT_ALLOWED", false)
	if err != nil {
		return Config{}, err
	}

	var need missing
	cfg := Config{
		CacheDir:              getEnvOrDefault("CACHE_DIR", defaultCacheDir),
		Port:                  getEnvOrDefault("PORT", defaultPort),
		RedirectPort:          getEnvOrDefault("HTTPS_REDIRECT_PORT", defaultRedirectPort),
		MetricsPort:           getEnvOrDefault("METRICS_PORT", defaultMetricsPort),
		DBInClusterEvaluation: os.Getenv("DB_INCLUSTER_EVALUATION") == "true",
		PublicBaseURL:         need.require("PUBLIC_BASE_URL"),
		ReservedLabels:        splitLowerTrimmed(os.Getenv("RESERVED_LABELS")),
		SecureMode:            secureMode,
		Backup: BackupConfig{
			Endpoint:        need.require("BACKUP_STORAGE_ENDPOINT"),
			Region:          getEnvOrDefault("BACKUP_STORAGE_REGION", defaultBackupRegion),
			Bucket:          need.require("BACKUP_STORAGE_BUCKET"),
			Prefix:          getEnvOrDefault("BACKUP_STORAGE_PREFIX", defaultBackupPrefix),
			AccessKeyID:     secretOrEmpty("BACKUP_STORAGE_ACCESS_KEY_ID", &secretErr),
			SecretAccessKey: secretOrEmpty("BACKUP_STORAGE_SECRET_ACCESS_KEY", &secretErr),
			InsecureAllowed: backupInsecure,
			SSE:             getEnvOrDefault("BACKUP_SSE", defaultBackupSSE),
			SSEKMSKeyID:     os.Getenv("BACKUP_SSE_KEY_ID"),

			EnvelopePlaintextAllowed: envelopePlaintextAllowed,
		},
	}
	cfg.OAuthRedirectHosts = splitLowerTrimmed(getEnvOrDefault("OAUTH_REDIRECT_HOSTS", defaultOAuthRedirectHosts))
	dsn, dbMissing, dsnErr := serverDatabaseDSN()
	if dsnErr != nil {
		return Config{}, dsnErr
	}
	need = append(need, dbMissing...)
	cfg.DBDSN = dsn

	cfg.OIDC = OIDCConfig{
		Issuer:              need.require("OIDC_ISSUER"),
		ClientID:            need.require("OIDC_CLIENT_ID"),
		ClientSecret:        need.requireSecret("OIDC_CLIENT_SECRET", &secretErr),
		Scopes:              strings.Fields(getEnvOrDefault("OIDC_SCOPES", defaultOIDCScopes)),
		EmailClaim:          getEnvOrDefault("OIDC_EMAIL_CLAIM", defaultOIDCEmailClaim),
		UsernameClaim:       os.Getenv("OIDC_USERNAME_CLAIM"),
		AdminClaim:          os.Getenv("OIDC_ADMIN_CLAIM"),
		AdminValue:          os.Getenv("OIDC_ADMIN_VALUE"),
		AdminEmails:         splitLowerTrimmed(os.Getenv("ADMIN_EMAILS")),
		AllowedEmailDomains: splitLowerTrimmed(os.Getenv("ALLOWED_EMAIL_DOMAINS")),
		HintDomain:          strings.TrimSpace(os.Getenv("OIDC_HINT_DOMAIN")),
		InsecureAllowed:     oidcInsecureAllowed,
	}

	signingKeys, err := parseKeys(secretOrEmpty("SESSION_SIGNING_KEY", &secretErr), signingKeyLength, maxSigningKeys)
	if err != nil {
		return Config{}, fmt.Errorf("SESSION_SIGNING_KEY: %w", err)
	}
	if len(signingKeys) == 0 {
		need = append(need, "SESSION_SIGNING_KEY")
	}
	sessionTTL, err := durationEnv("SESSION_TTL", defaultSessionTTL)
	if err != nil {
		return Config{}, err
	}
	sessionIdle, err := durationEnv("SESSION_IDLE", defaultSessionIdle)
	if err != nil {
		return Config{}, err
	}
	if sessionTTL > maxSessionTTL {
		return Config{}, fmt.Errorf("SESSION_TTL must be at most %s, got %s", maxSessionTTL, sessionTTL)
	}
	if sessionIdle > maxSessionIdle {
		return Config{}, fmt.Errorf("SESSION_IDLE must be at most %s, got %s", maxSessionIdle, sessionIdle)
	}
	if sessionIdle > sessionTTL {
		return Config{}, fmt.Errorf("SESSION_IDLE (%s) must not be longer than SESSION_TTL (%s)", sessionIdle, sessionTTL)
	}
	cfg.OAuthAccessTTL, err = durationEnv("OAUTH_ACCESS_TTL", defaultOAuthAccessTTL)
	if err != nil {
		return Config{}, err
	}
	if cfg.OAuthAccessTTL > maxOAuthAccessTTL {
		return Config{}, fmt.Errorf("OAUTH_ACCESS_TTL must be at most %s, got %s", maxOAuthAccessTTL, cfg.OAuthAccessTTL)
	}
	cfg.OAuthRefreshTTL, err = durationEnv("OAUTH_REFRESH_TTL", defaultOAuthRefreshTTL)
	if err != nil {
		return Config{}, err
	}
	if cfg.OAuthRefreshTTL > maxOAuthRefreshTTL {
		return Config{}, fmt.Errorf("OAUTH_REFRESH_TTL must be at most %s (90 days), got %s", maxOAuthRefreshTTL, cfg.OAuthRefreshTTL)
	}
	if cfg.OAuthAccessTTL > cfg.OAuthRefreshTTL {
		return Config{}, fmt.Errorf("OAUTH_ACCESS_TTL (%s) must not be longer than OAUTH_REFRESH_TTL (%s)", cfg.OAuthAccessTTL, cfg.OAuthRefreshTTL)
	}
	cfg.Session = SessionConfig{
		SigningKeys: toSigningKeys(signingKeys),
		TTL:         sessionTTL,
		Idle:        sessionIdle,
	}

	assetMaxFileBytes, err := int64Env("ASSET_MAX_FILE_BYTES", defaultAssetMaxFileBytes)
	if err != nil {
		return Config{}, err
	}
	assetMaxSiteBytes, err := int64Env("ASSET_MAX_SITE_BYTES", defaultAssetMaxSiteBytes)
	if err != nil {
		return Config{}, err
	}
	assetMaxSiteCount, err := int64Env("ASSET_MAX_SITE_COUNT", defaultAssetMaxSiteCount)
	if err != nil {
		return Config{}, err
	}
	cfg.CacheMaxBytes, err = int64Env("CACHE_MAX_BYTES", defaultCacheMaxBytes)
	if err != nil {
		return Config{}, err
	}
	cfg.Assets = AssetLimits{
		MaxFileBytes: assetMaxFileBytes,
		MaxSiteBytes: assetMaxSiteBytes,
		MaxSiteCount: assetMaxSiteCount,
	}

	trusted, set := os.LookupEnv("TRUSTED_PROXY_CIDRS")
	if !set {
		// Unset means the usual in-cluster case: the ingress controller sits
		// on a private address. Set it (even to "") to override.
		trusted = defaultTrustedProxies
	}
	cfg.TrustedProxies, err = parseTrustedProxies(trusted)
	if err != nil {
		return Config{}, fmt.Errorf("TRUSTED_PROXY_CIDRS: %w", err)
	}
	cfg.APIKeyMaxDays, err = int64Env("API_KEY_MAX_DAYS", maxAPIKeyDays)
	if err != nil {
		return Config{}, err
	}
	if cfg.APIKeyMaxDays < 1 || cfg.APIKeyMaxDays > maxAPIKeyDays {
		return Config{}, fmt.Errorf("API_KEY_MAX_DAYS must be between 1 and %d, got %d", maxAPIKeyDays, cfg.APIKeyMaxDays)
	}

	// NETWORK_ACCESS_APPROVALS: one admin (the default) or two different
	// admins approve a request for network access. Nothing else is accepted.
	approvals, err := int64Env("NETWORK_ACCESS_APPROVALS", 1)
	if err != nil {
		return Config{}, err
	}
	if approvals != 1 && approvals != 2 {
		return Config{}, fmt.Errorf("NETWORK_ACCESS_APPROVALS must be 1 or 2, got %d", approvals)
	}
	cfg.NetworkAccessApprovals = int(approvals)

	auditCfg, err := LoadAuditRetention()
	if err != nil {
		return Config{}, err
	}
	cfg.Audit = auditCfg

	if err := need.err(); err != nil {
		return Config{}, err
	}
	if secretErr != nil {
		return Config{}, secretErr
	}

	if err := validateOIDCIssuer(cfg.OIDC.Issuer, cfg.OIDC.InsecureAllowed); err != nil {
		return Config{}, fmt.Errorf("OIDC_ISSUER: %w", err)
	}
	if len(cfg.OIDC.AdminClaim) > 0 != (len(cfg.OIDC.AdminValue) > 0) {
		return Config{}, errors.New("OIDC_ADMIN_CLAIM and OIDC_ADMIN_VALUE must be set together")
	}
	if cfg.OIDC.HintDomain == "" && len(cfg.OIDC.AllowedEmailDomains) == 1 {
		cfg.OIDC.HintDomain = cfg.OIDC.AllowedEmailDomains[0]
	}

	publicBaseURL, err := validatePublicBaseURL(cfg.PublicBaseURL, cfg.SecureMode)
	if err != nil {
		return Config{}, fmt.Errorf("PUBLIC_BASE_URL: %w", err)
	}
	cfg.PublicBaseURL = publicBaseURL
	if cfg.SecureMode && cfg.RedirectPort == cfg.Port {
		return Config{}, errors.New("HTTPS_REDIRECT_PORT must differ from PORT in secure mode")
	}
	if cfg.MetricsPort == cfg.Port || (cfg.SecureMode && cfg.MetricsPort == cfg.RedirectPort) {
		return Config{}, errors.New("METRICS_PORT must differ from PORT and HTTPS_REDIRECT_PORT")
	}
	if err := validateBackupEndpoint(cfg.Backup); err != nil {
		return Config{}, fmt.Errorf("BACKUP_STORAGE_ENDPOINT: %w", err)
	}
	if (cfg.Backup.AccessKeyID == "") != (cfg.Backup.SecretAccessKey == "") {
		return Config{}, errors.New("BACKUP_STORAGE_ACCESS_KEY_ID and BACKUP_STORAGE_SECRET_ACCESS_KEY must be set together")
	}
	if err := validateBackupSSE(cfg.Backup); err != nil {
		return Config{}, fmt.Errorf("BACKUP_SSE: %w", err)
	}
	envelopeKeys, err := parseEnvelopeKeys(secretOrEmpty("BACKUP_ENVELOPE_KEY", &secretErr))
	if err != nil {
		return Config{}, fmt.Errorf("BACKUP_ENVELOPE_KEY: %w", err)
	}
	// Checked again here: an unreadable BACKUP_ENVELOPE_KEY_FILE must stop
	// startup, not quietly leave backups without their envelope.
	if secretErr != nil {
		return Config{}, secretErr
	}
	cfg.Backup.EnvelopeKeys = envelopeKeys
	if err := validateDatabaseSSL(cfg.DBDSN, dbInsecureAllowed); err != nil {
		return Config{}, fmt.Errorf("database TLS: %w", err)
	}
	return cfg, nil
}

// parseTrustedProxies reads a comma-separated list of CIDRs (a bare address
// counts as a single-address prefix).
// defaultTrustedProxies are the private and carrier-grade NAT ranges an
// ingress controller or cloud load balancer reaches the pod from.
const defaultTrustedProxies = "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,100.64.0.0/10,fc00::/7"

func parseTrustedProxies(raw string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if prefix, err := netip.ParsePrefix(item); err == nil {
			out = append(out, prefix.Masked())
			continue
		}
		addr, err := netip.ParseAddr(item)
		if err != nil {
			return nil, fmt.Errorf("%q is not a CIDR or an IP address", item)
		}
		out = append(out, netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen()))
	}
	return out, nil
}

// validateOIDCIssuer refuses Entra ID's multi-tenant endpoints: with
// /common, /organizations or /consumers any Microsoft account from any
// tenant can sign in, and the email it presents is whatever that tenant
// says. Use the tenant-specific issuer
// (https://login.microsoftonline.com/<tenant id>/v2.0).
func validateOIDCIssuer(issuer string, insecureAllowed bool) error {
	u, err := url.Parse(strings.ToLower(strings.TrimSpace(issuer)))
	if err != nil || u.Host == "" {
		return errors.New("must be an absolute URL")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && insecureAllowed) {
		return errors.New("must use https (OIDC_INSECURE_ALLOWED=true permits http on a local evaluation cluster only)")
	}
	if u.Host != "login.microsoftonline.com" {
		return nil
	}
	tenant, _, _ := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
	switch tenant {
	case "common", "organizations", "consumers":
		return fmt.Errorf("the multi-tenant Entra ID endpoint /%s is refused; use your tenant's own issuer, https://login.microsoftonline.com/<tenant id>/v2.0", tenant)
	}
	return nil
}

// LoadDatabase returns only the database DSN. The migrate subcommand runs in
// an init container with the same environment as the server but needs nothing
// else, and must not fail over an unrelated missing value.
func LoadDatabase() (string, error) {
	dsn, need, err := databaseDSN()
	if err != nil {
		return "", err
	}
	if err := missing(need).err(); err != nil {
		return "", err
	}
	dbInsecureAllowed, err := boolEnv("DB_INSECURE_ALLOWED", false)
	if err != nil {
		return "", err
	}
	if err := validateDatabaseSSL(dsn, dbInsecureAllowed); err != nil {
		return "", fmt.Errorf("database TLS: %w", err)
	}
	return dsn, nil
}

// LoadAppRolePassword returns the password the migrate subcommand sets on the
// least-privilege application role. It is required whenever
// migrate runs: a role granted in a migration with no password to give it
// would sit unusable, and a silently-skipped step is worse than a startup
// failure that names what is missing.
func LoadAppRolePassword() (string, error) {
	var secretErr error
	var need missing
	password := need.requireSecret("DB_APP_PASSWORD", &secretErr)
	if err := need.err(); err != nil {
		return "", err
	}
	if secretErr != nil {
		return "", secretErr
	}
	if err := refuseSharedDatabasePassword(password); err != nil {
		return "", err
	}
	return password, nil
}

// refuseSharedDatabasePassword refuses an application-role password equal to
// the owning role's — the two roles never share one — checked
// wherever both may be present.
func refuseSharedDatabasePassword(appPassword string) error {
	owner, err := secretEnv("DB_PASSWORD")
	if err != nil {
		return nil // an unreadable owner password is databaseDSN's error to report
	}
	if owner = strings.TrimSpace(owner); owner != "" && owner == appPassword {
		return errors.New("DB_PASSWORD and DB_APP_PASSWORD must differ: the application role must not share the owning role's password")
	}
	return nil
}

// serverDatabaseDSN is the DSN the server itself connects with. With
// DB_APP_USER set it is built from the DB_HOST/DB_NAME parts plus
// DB_APP_USER and DB_APP_PASSWORD (or DB_APP_PASSWORD_FILE), so the owning
// role's DB_PASSWORD/DB_PASSWORD_FILE never decides the server's
// credential. Without it, the server uses DB_DSN or DB_USER/DB_PASSWORD as
// before; either way the server checks at startup that the role it got
// cannot rewrite audit_events (migrate.CheckLeastPrivilege).
func serverDatabaseDSN() (string, []string, error) {
	appUser := strings.TrimSpace(os.Getenv("DB_APP_USER"))
	if appUser == "" {
		return databaseDSN()
	}
	if strings.TrimSpace(os.Getenv("DB_DSN")) != "" {
		return "", nil, errors.New("DB_APP_USER cannot be combined with DB_DSN; put the application role in DB_DSN itself, or use the DB_HOST/DB_NAME parts")
	}
	var need missing
	var secretErr error
	password := need.requireSecret("DB_APP_PASSWORD", &secretErr)
	if secretErr != nil {
		return "", nil, secretErr
	}
	if password != "" {
		if err := refuseSharedDatabasePassword(password); err != nil {
			return "", nil, err
		}
	}
	return databaseDSNAs(appUser, password, need)
}

// LoadAuditRetention reads the three retention/visibility
// settings on their own, the same narrow-loader shape LoadDatabase and
// LoadAppRolePassword use: `simple-host prune` (cmd/server/subcommands.go)
// needs only these three values, plus config.LoadDatabase's own DSN, never
// the rest of Load's required application settings (PUBLIC_BASE_URL, the
// OIDC provider, and so on) — a CronJob that shares the deployment's
// ConfigMap and Secret happens to have those too, but a prune-only
// environment should not be forced to set them just to run a retention
// sweep. Load calls this too, so the full server sees the same values.
func LoadAuditRetention() (AuditConfig, error) {
	auditRetentionDays, err := int64Env("AUDIT_RETENTION_DAYS", defaultAuditRetentionDays)
	if err != nil {
		return AuditConfig{}, err
	}
	accessLogRetentionDays, err := int64Env("ACCESS_LOG_RETENTION_DAYS", defaultAccessLogRetentionDays)
	if err != nil {
		return AuditConfig{}, err
	}
	accessLogVisibility := getEnvOrDefault("ACCESS_LOG_VISIBILITY", defaultAccessLogVisibility)
	if accessLogVisibility != "counts" && accessLogVisibility != "owner" && accessLogVisibility != "admin" {
		return AuditConfig{}, fmt.Errorf("ACCESS_LOG_VISIBILITY must be %q, %q or %q, got %q", "counts", "owner", "admin", accessLogVisibility)
	}
	return AuditConfig{
		RetentionDays:          auditRetentionDays,
		AccessLogRetentionDays: accessLogRetentionDays,
		AccessLogVisibility:    accessLogVisibility,
	}, nil
}

// databaseDSN accepts either a complete DB_DSN or the parts. The parts are the
// normal shape in Kubernetes, where only the password lives in a Secret and
// the rest is plain configuration; the DSN form exists for local development.
//
// DB_DSN is accepted only in URL form (scheme postgres:// or postgresql://)
// and is re-serialized once here via url.Parse + URL.String() before it is
// used anywhere else: validateDatabaseSSL and the eventual sql.Open both see
// exactly this string, never the operator's original one. See
// normalizeDatabaseDSNURL for why the keyword/value form
// ("host=... sslmode=...") lib/pq also accepts is refused outright rather
// than merely tolerated.
func databaseDSN() (string, []string, error) {
	if raw := strings.TrimSpace(os.Getenv("DB_DSN")); raw != "" {
		normalized, err := normalizeDatabaseDSNURL(raw)
		if err != nil {
			return "", nil, fmt.Errorf("DB_DSN: %w", err)
		}
		return normalized, nil, nil
	}
	var need missing
	var secretErr error
	user := need.require("DB_USER")
	password := need.requireSecret("DB_PASSWORD", &secretErr)
	if secretErr != nil {
		return "", nil, secretErr
	}
	return databaseDSNAs(user, password, need)
}

// databaseDSNAs builds the parts-form DSN for user/password; need carries
// whatever the caller already found missing.
func databaseDSNAs(user, password string, need missing) (string, []string, error) {
	host := need.require("DB_HOST")
	name := need.require("DB_NAME")
	if len(need) > 0 {
		return "", []string{"DB_DSN or DB_HOST, DB_USER, DB_PASSWORD, DB_NAME (missing: " + strings.Join(need, ", ") + ")"}, nil
	}
	query := url.Values{}
	query.Set("sslmode", getEnvOrDefault("DB_SSLMODE", defaultDBSSLMode))
	if rootCert := os.Getenv("DB_SSL_ROOT_CERT"); rootCert != "" {
		query.Set("sslrootcert", rootCert)
	}
	dsn := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, password),
		Host:     host + ":" + getEnvOrDefault("DB_PORT", defaultDBPort),
		Path:     "/" + name,
		RawQuery: query.Encode(),
	}
	return dsn.String(), nil, nil
}

// normalizeDatabaseDSNURL accepts only the URL form of a Postgres DSN —
// "postgres://[user[:password]@]host[:port]/dbname?sslmode=...&sslrootcert=..."
// — and returns it re-parsed and re-serialized, so the exact string
// validateDatabaseSSL checks is byte-for-byte the string used to connect.
//
// lib/pq also accepts a keyword/value DSN ("host=... sslmode=disable ..."),
// and the previous version of this check ran url.Parse on whatever DB_DSN
// held regardless of shape. Two things went wrong with that: a correct
// keyword/value DSN was wrongly refused (url.Parse finds no "?" query
// component in it at all, so sslmode reads as empty, not "verify-full");
// and, worse, a crafted one was wrongly accepted — url.Parse splits a
// string on its first unescaped "?" whether or not a scheme precedes it, so
// a keyword/value DSN whose password (or any other quoted field) happens to
// contain literal text shaped like "?sslmode=verify-full&sslrootcert=/x"
// hands the checker exactly the values it wants to see, while the DSN's own
// real, explicit "sslmode=disable" keyword elsewhere in the string is what
// lib/pq actually connects with. Refusing every shape but a genuine
// postgres:// URL closes both: this parser has no notion of a query string
// that isn't delimited by "://" and "?" the way URL syntax actually
// requires, so there is nothing in a keyword/value string for it to
// misread.
func normalizeDatabaseDSNURL(raw string) (string, error) {
	const wantShape = `must be a URL of the form "postgres://user:password@host:port/dbname?sslmode=verify-full&sslrootcert=...", not a keyword/value DSN`
	parsed, err := url.Parse(raw)
	if err != nil {
		// Not wrapped: url.Parse's error quotes the whole input, password
		// included, and this message goes to the pod log.
		return "", errors.New("could not be parsed as a URL (value not shown: it may contain a password)")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "postgres" && scheme != "postgresql" {
		return "", errors.New(wantShape)
	}
	if parsed.Opaque != "" || parsed.Host == "" {
		return "", errors.New(wantShape)
	}
	return parsed.String(), nil
}

// validateDatabaseSSL refuses a DSN whose sslmode is anything but
// verify-full, and refuses verify-full with no sslrootcert to check the
// server certificate against, unless insecureAllowed is set. It is the one
// required verify-full check, applied wherever a DSN is produced: databaseDSN's
// own default is already verify-full, but a DSN supplied whole through
// DB_DSN, or a DB_SSLMODE override, bypasses that default and must be
// checked here instead. By the time a DSN reaches this function it has
// already been through normalizeDatabaseDSNURL (DB_DSN) or built directly as
// a url.URL (the parts form), so parsing it again here is a faithful,
// unambiguous round trip rather than a second chance to misread it.
func validateDatabaseSSL(dsn string, insecureAllowed bool) error {
	if insecureAllowed {
		return nil
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		return errors.New("parse DSN: not a valid URL (value not shown)")
	}
	query := parsed.Query()
	sslmode := query.Get("sslmode")
	if !strings.EqualFold(sslmode, "verify-full") {
		return fmt.Errorf(
			"sslmode must be verify-full, got %q (DB_INSECURE_ALLOWED=true permits a weaker mode for local evaluation only)",
			sslmode,
		)
	}
	if strings.TrimSpace(query.Get("sslrootcert")) == "" {
		return errors.New(
			"sslrootcert is required when sslmode=verify-full (DB_INSECURE_ALLOWED=true permits omitting it for local evaluation only)",
		)
	}
	return nil
}

// validateBackupSSE checks the server-side-encryption mode against the two
// values S3-compatible stores understand, and that the KMS key id is present
// exactly when the mode needs one.
func validateBackupSSE(b BackupConfig) error {
	switch b.SSE {
	case sseAES256:
		if b.SSEKMSKeyID != "" {
			return errors.New(`BACKUP_SSE_KEY_ID is only valid with BACKUP_SSE=aws:kms`)
		}
		return nil
	case sseKMS:
		if strings.TrimSpace(b.SSEKMSKeyID) == "" {
			return errors.New(`BACKUP_SSE_KEY_ID is required when BACKUP_SSE=aws:kms`)
		}
		return nil
	default:
		return fmt.Errorf("must be %q or %q, got %q", sseAES256, sseKMS, b.SSE)
	}
}

// parseEnvelopeKeys reads BACKUP_ENVELOPE_KEY: empty disables the client-side
// envelope; otherwise 1 to maxEnvelopeKeys comma-separated
// "<id>:<base64 32 bytes>" entries, the first of which wraps every new backup object while every entry is
// tried, by id, to unwrap an existing one (the same rotation shape used
// for SESSION_SIGNING_KEY).
func parseEnvelopeKeys(raw string) ([]EnvelopeKey, error) {
	parsed, err := parseKeys(raw, envelopeKeyLength, maxEnvelopeKeys)
	if err != nil {
		return nil, err
	}
	if len(parsed) == 0 {
		return nil, nil
	}
	keys := make([]EnvelopeKey, len(parsed))
	for i, k := range parsed {
		keys[i] = EnvelopeKey{ID: k.ID, Key: k.Key}
	}
	return keys, nil
}

func toSigningKeys(parsed []keyEntry) []SigningKey {
	if len(parsed) == 0 {
		return nil
	}
	keys := make([]SigningKey, len(parsed))
	for i, k := range parsed {
		keys[i] = SigningKey{ID: k.ID, Key: k.Key}
	}
	return keys
}

// keyEntry is the shared shape behind both BACKUP_ENVELOPE_KEY and
// SESSION_SIGNING_KEY: "<id>:<base64 N-byte key>", comma-separated, up to
// two, first-wins on write, every entry tried on read. Kept as one parser so
// the two env vars can never quietly drift in what they accept.
type keyEntry struct {
	ID  string
	Key []byte
}

func parseKeys(raw string, keyLength, maxKeys int) ([]keyEntry, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) > maxKeys {
		return nil, fmt.Errorf("at most %d keys are accepted, got %d", maxKeys, len(parts))
	}
	seen := make(map[string]bool, len(parts))
	keys := make([]keyEntry, 0, len(parts))
	for i, part := range parts {
		part = strings.TrimSpace(part)
		id, encoded, ok := strings.Cut(part, ":")
		id = strings.TrimSpace(id)
		if !ok || id == "" || encoded == "" {
			// The entry itself is never echoed: a bare key without its id
			// is the usual mistake, and this error goes to the pod log.
			return nil, fmt.Errorf("entry %d must be \"<id>:<base64 %d-byte key>\"", i+1, keyLength)
		}
		if seen[id] {
			return nil, fmt.Errorf("key id %q repeated", id)
		}
		seen[id] = true
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
		if err != nil {
			return nil, fmt.Errorf("key %q: decode base64: %w", id, err)
		}
		if len(key) != keyLength {
			return nil, fmt.Errorf("key %q must decode to %d bytes, got %d", id, keyLength, len(key))
		}
		keys = append(keys, keyEntry{ID: id, Key: key})
	}
	return keys, nil
}

// splitLowerTrimmed parses a comma-separated env var into a lowercased,
// trimmed slice, dropping empty entries. Used for ADMIN_EMAILS and
// ALLOWED_EMAIL_DOMAINS, neither of which is required, so an unset var
// yields nil rather than an error.
func splitLowerTrimmed(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func durationEnv(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %s", key, raw)
	}
	return d, nil
}

// int64Env parses a positive integer environment variable (bytes or a
// count), the same fallback-when-unset, error-when-invalid shape
// durationEnv uses for a time.Duration.
func int64Env(key string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if value <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %s", key, raw)
	}
	return value, nil
}

func validateBackupEndpoint(b BackupConfig) error {
	parsed, err := url.Parse(b.Endpoint)
	if err != nil {
		return fmt.Errorf("parse URL: %w", err)
	}
	if parsed.Host == "" || parsed.Hostname() == "" {
		return errors.New("must be an absolute URL with a host")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		return nil
	case "http":
		if b.InsecureAllowed {
			return nil
		}
		return errors.New("must use https (BACKUP_STORAGE_INSECURE_ALLOWED=true permits http on a local cluster only)")
	default:
		return errors.New("scheme must be http or https")
	}
}

// validatePublicBaseURL returns a normalized root-path origin. Secure mode is
// deliberately strict because the value is the sole authority used for HTTP
// redirects; request Host and forwarding headers are never trusted.
func validatePublicBaseURL(raw string, secureMode bool) (string, error) {
	if strings.ContainsRune(raw, '#') {
		return "", errors.New("must not contain a fragment")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		// Not wrapped: url.Parse's error quotes the whole input, password
		// included, and this message goes to the pod log.
		return "", errors.New("could not be parsed as a URL (value not shown: it may contain a password)")
	}
	if parsed.Scheme == "" || parsed.Host == "" || parsed.Hostname() == "" || !parsed.IsAbs() {
		return "", errors.New("must be an absolute URL with a host")
	}
	if parsed.Opaque != "" || parsed.User != nil {
		return "", errors.New("must not contain opaque data or user credentials")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", errors.New("must not contain a query or fragment")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", errors.New("must use the origin root path")
	}
	if parsed.RawPath != "" && parsed.EscapedPath() != "/" {
		return "", errors.New("must use the origin root path")
	}
	if secureMode && !strings.EqualFold(parsed.Scheme, "https") {
		return "", errors.New("must use https in secure mode")
	}
	if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return "", errors.New("scheme must be http or https")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Path = ""
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func boolEnv(key string, fallback bool) (bool, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false, fmt.Errorf("%s must be true or false: %w", key, err)
	}
	return value, nil
}

// secretEnv reads a secret from KEY, or from the file named by KEY_FILE when
// that is set. The file wins, and a KEY_FILE that cannot be read is fatal
// rather than silently empty — an unreadable credential must not look like an
// unset one.
//
// This is the cloud-neutral way to get a credential into the pod. Every cloud
// has its own workload-identity flow and they agree on nothing, but every
// cloud's secret store has a Kubernetes-native projection — the Secrets Store
// CSI driver, or External Secrets Operator writing a Secret — and both of
// those land a file in the container. A file also keeps the value out of the
// environment, where it is visible to every child process, every crash dump
// and anything that prints its own env.
// secretOrEmpty is secretEnv for an optional value: a read failure is recorded
// in fail and the caller carries on collecting, so one broken mount reports
// alongside everything else rather than short-circuiting the sweep.
func secretOrEmpty(key string, fail *error) string {
	value, err := secretEnv(key)
	if err != nil && *fail == nil {
		*fail = err
	}
	return value
}

func secretEnv(key string) (string, error) {
	if path := strings.TrimSpace(os.Getenv(key + "_FILE")); path != "" {
		body, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("%s_FILE: %w", key, err)
		}
		return strings.TrimSpace(string(body)), nil
	}
	return os.Getenv(key), nil
}

func getEnvOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
