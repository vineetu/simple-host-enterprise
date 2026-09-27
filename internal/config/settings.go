package config

import (
	"strconv"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/oplimits"
)

// The settings registry: every environment setting this package reads, with
// its area, a plain description, type, default and range. `simple-host
// settings --json` prints it (cmd/server adds the rate-limit defaults, which
// live in internal/handler); docs/advanced/settings.json is that output, and
// the docs/advanced tables and the hosted setup helper (simple-host.app/setup)
// are built from that file. A test fails when a variable this package reads
// is missing here, or when the file drifts. Refresh with
// `go run ./cmd/server settings --json > docs/advanced/settings.json &&
// python3 scripts/settings_docs.py`.

// SettingGroup is one area of the advanced docs (docs/advanced/<Page>).
type SettingGroup struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Page string `json:"page"`
}

// Setting is one environment setting.
type Setting struct {
	Name        string `json:"name"`
	Group       string `json:"group"`
	Description string `json:"description"`
	// Type is int, duration, bool, enum, string, secret or rate. A secret
	// belongs in the simple-host-secrets Secret, everything else in the
	// simple-host-config ConfigMap (config.env).
	Type    string   `json:"type"`
	Default string   `json:"default"`
	Min     any      `json:"min,omitempty"`
	Max     any      `json:"max,omitempty"`
	Allowed []string `json:"allowed,omitempty"`
	Unit    string   `json:"unit,omitempty"`
	// Loosest is the loosest value a security-sensitive rate limit accepts.
	Loosest string `json:"loosest,omitempty"`
	// Security: loosening it, or leaking it, weakens sign-in or data safety.
	Security bool `json:"security_sensitive"`
	// Required: the server does not start without it.
	Required bool `json:"required"`
	// Basic: asked in the setup helper's basic mode.
	Basic bool `json:"basic"`
}

// SettingsDoc is the whole registry as `settings --json` prints it.
type SettingsDoc struct {
	Product    string         `json:"product"`
	RateFormat string         `json:"rate_format"`
	Groups     []SettingGroup `json:"groups"`
	Settings   []Setting      `json:"settings"`
}

// SettingGroups is every area, in the order the docs give them.
var SettingGroups = []SettingGroup{
	{"server", "Server and addresses", "server-and-addresses.md"},
	{"accounts", "Accounts and sign-in", "accounts-and-sign-in.md"},
	{"access", "Access and teams", "access-and-teams.md"},
	{"sites", "Sites and versions", "sites-and-versions.md"},
	{"data", "Saved data", "saved-data.md"},
	{"cleanup", "Cleanup and retention", "cleanup-and-retention.md"},
	{"email", "Email", "email.md"},
	{"storage", "Storage and backups", "storage-and-backups.md"},
	{"observability", "Observability", "observability.md"},
}

// RateLimitDocs are the area and description of each RATE_LIMIT_<NAME>
// (RateLimitNames); cmd/server adds the defaults from internal/handler.
var RateLimitDocs = map[string][2]string{
	"auth-client":          {"accounts", "Sign-in, the session hand-off and AI app authorization, per client address (counted across replicas)."},
	"auth-email":           {"accounts", "Sign-in attempts per email address."},
	"management-client":    {"sites", "Management API calls per client address."},
	"management-user":      {"sites", "Management changes per person."},
	"api-key-mint":         {"accounts", "API keys minted per person (counted across replicas)."},
	"state-client":         {"data", "Saved-data writes per client."},
	"state-site":           {"data", "Saved-data writes per site."},
	"state-read-client":    {"data", "Saved-data reads per client."},
	"state-read-site":      {"data", "Saved-data reads per site."},
	"admin-client":         {"access", "Admin actions per client address."},
	"admin-identity":       {"access", "Admin actions per admin."},
	"search-query-peer":    {"sites", "Site searches per network peer."},
	"search-query-session": {"sites", "Site searches per search session."},
	"oauth-register":       {"accounts", "AI app registrations per client address (counted across replicas)."},
	"oauth-token":          {"accounts", "AI app token requests per client address (counted across replicas)."},
}

// RateLimitEnv is the variable that sets the named limit.
func RateLimitEnv(name string) string {
	return rateLimitPrefix + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// ShortDuration writes 8h, 30m, 1.25s rather than 8h0m0s.
func ShortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// Settings is every setting this package reads, in SettingGroups order,
// with the rate limits' Default and Loosest left for cmd/server to fill.
func Settings() []Setting {
	op := oplimits.Defaults()
	d := ShortDuration
	all := []Setting{
		// Server and addresses.
		{Name: "PUBLIC_BASE_URL", Group: "server", Type: "string", Required: true, Basic: true,
			Description: "The address people open, like https://sites.example.com: its own registrable domain, never a subdomain of the company's main one. Every site is served at <site>.<owner>.<host>."},
		{Name: "SECURE_MODE", Group: "server", Type: "bool", Default: "false", Allowed: []string{"true", "false"}, Security: true,
			Description: "true requires HTTPS for PUBLIC_BASE_URL and a separate redirect port. Every real install sets it."},
		{Name: "PORT", Group: "server", Type: "int", Default: defaultPort, Min: 1, Max: 65535,
			Description: "The port the server listens on."},
		{Name: "HTTPS_REDIRECT_PORT", Group: "server", Type: "int", Default: defaultRedirectPort, Min: 1, Max: 65535,
			Description: "The port that redirects plain HTTP to HTTPS. Must differ from PORT with SECURE_MODE."},
		{Name: "TRUSTED_PROXY_CIDRS", Group: "server", Type: "string", Default: defaultTrustedProxies, Security: true,
			Description: "The proxies in front of the server (the ingress controller's pods), so the client address comes from X-Forwarded-For. Empty trusts none."},
		{Name: "OWNER_CERTS", Group: "server", Type: "enum", Default: "auto", Allowed: []string{"auto", "manual"},
			Description: "auto: each owner's sites move to their own hosts once the owner-hosts reconciler has their certificate. manual: you issue those certificates yourself."},
		{Name: "OWNER_CERT_ISSUER", Group: "server", Type: "string", Basic: true,
			Description: "The cert-manager ClusterIssuer that signs each owner's *.<owner>.<host> certificate (an internal CA, or ACME with DNS-01). Needed by the reconciler."},
		{Name: "OWNER_INGRESS_TEMPLATE", Group: "server", Type: "string", Default: "simple-host",
			Description: "The install's own Ingress, which each owner's Ingress copies its class and annotations from."},
		{Name: "OWNER_HOSTS_INTERVAL", Group: "server", Type: "duration", Default: "15s", Unit: "Go duration",
			Description: "How often the reconciler applies owner Ingresses and records certificate readiness."},
		{Name: "RESERVED_LABELS", Group: "server", Type: "string",
			Description: "Extra hostnames (comma-separated) no person or team may take as a name, on top of the built-in ones."},

		// Accounts and sign-in.
		{Name: "OIDC_ISSUER", Group: "accounts", Type: "string", Required: true, Basic: true, Security: true,
			Description: "Your identity provider's issuer URL (Okta, Entra ID tenant, Google, Keycloak, ...). People sign in only through it."},
		{Name: "OIDC_CLIENT_ID", Group: "accounts", Type: "string", Required: true, Basic: true,
			Description: "The OIDC client ID registered for this install."},
		{Name: "OIDC_CLIENT_SECRET", Group: "accounts", Type: "secret", Required: true, Basic: true, Security: true,
			Description: "The OIDC client secret."},
		{Name: "OIDC_SCOPES", Group: "accounts", Type: "string", Default: defaultOIDCScopes,
			Description: "Scopes asked for at sign-in, space-separated."},
		{Name: "OIDC_EMAIL_CLAIM", Group: "accounts", Type: "string", Default: defaultOIDCEmailClaim,
			Description: "The ID token claim read as the person's email address. The provider must vouch that it is verified."},
		{Name: "OIDC_USERNAME_CLAIM", Group: "accounts", Type: "string",
			Description: "A claim to derive usernames from. Empty: the part of the email before the @."},
		{Name: "OIDC_ADMIN_CLAIM", Group: "accounts", Type: "string", Security: true,
			Description: "A claim that makes someone an admin when it holds OIDC_ADMIN_VALUE. Set both or neither."},
		{Name: "OIDC_ADMIN_VALUE", Group: "accounts", Type: "string", Security: true,
			Description: "The value of OIDC_ADMIN_CLAIM that makes someone an admin."},
		{Name: "OIDC_INSECURE_ALLOWED", Group: "accounts", Type: "bool", Default: "false", Allowed: []string{"true", "false"}, Security: true,
			Description: "Allows a plain-HTTP identity provider. Local evaluation only; never on a real install."},
		{Name: "ADMIN_EMAILS", Group: "accounts", Type: "string", Basic: true, Security: true,
			Description: "Admins' email addresses, comma-separated. Removing one demotes them on the next deploy."},
		{Name: "ALLOWED_EMAIL_DOMAINS", Group: "accounts", Type: "string", Basic: true, Security: true,
			Description: "Email domains that may sign in, comma-separated. Required with Google, where anyone has an account."},
		{Name: "OIDC_HINT_DOMAIN", Group: "accounts", Type: "string", Default: "<the only ALLOWED_EMAIL_DOMAINS entry>",
			Description: "The domain hint sent to the provider, to narrow its account chooser. It never authorizes anyone."},
		{Name: "OAUTH_REDIRECT_HOSTS", Group: "accounts", Type: "string", Default: defaultOAuthRedirectHosts, Security: true,
			Description: "Where an AI app connecting to /mcp may be sent back after sign-in, comma-separated."},
		{Name: "SESSION_SIGNING_KEY", Group: "accounts", Type: "secret", Required: true, Security: true,
			Description: "Signs the sign-in cookie: one or two <id>:<base64 32-byte key> entries, the first signing. Generate with echo \"k1:$(openssl rand -base64 32)\"."},
		{Name: "SESSION_TTL", Group: "accounts", Type: "duration", Default: d(defaultSessionTTL), Max: d(maxSessionTTL), Unit: "Go duration", Security: true,
			Description: "How long a sign-in lasts, however active. Match your identity provider's session policy."},
		{Name: "SESSION_IDLE", Group: "accounts", Type: "duration", Default: d(defaultSessionIdle), Max: d(maxSessionIdle), Unit: "Go duration", Security: true,
			Description: "How long a sign-in survives unused. Not longer than SESSION_TTL."},
		{Name: "OAUTH_ACCESS_TTL", Group: "accounts", Type: "duration", Default: d(defaultOAuthAccessTTL), Max: d(maxOAuthAccessTTL), Unit: "Go duration", Security: true,
			Description: "How long an AI app's access token works before it refreshes."},
		{Name: "OAUTH_REFRESH_TTL", Group: "accounts", Type: "duration", Default: d(defaultOAuthRefreshTTL), Max: d(maxOAuthRefreshTTL), Unit: "Go duration", Security: true,
			Description: "How long an AI app stays connected before the person signs in at the identity provider again."},
		{Name: "API_KEY_MAX_DAYS", Group: "accounts", Type: "int", Default: itoa(maxAPIKeyDays), Min: 1, Max: maxAPIKeyDays, Unit: "days", Security: true,
			Description: "The longest lifetime an API key (for CI and automation) may be minted with."},
		{Name: "API_KEY_DEFAULT_DAYS", Group: "accounts", Type: "int", Default: itoa(int64(op.APIKeyDefaultDays)), Min: 1, Max: maxAPIKeyDays, Unit: "days", Security: true,
			Description: "A new API key's lifetime when none is asked for. At most API_KEY_MAX_DAYS."},
		{Name: "API_KEY_EXPIRY_WARNING_DAYS", Group: "accounts", Type: "int", Default: itoa(int64(op.APIKeyExpiryWarningDays)), Min: 1, Max: maxAPIKeyExpiryWarningDays, Unit: "days",
			Description: "A key this close to expiry shows \"expires soon\" and warns on every call."},

		// Access and teams.
		{Name: "NETWORK_ACCESS_APPROVALS", Group: "access", Type: "enum", Default: "1", Allowed: []string{"1", "2"}, Security: true,
			Description: "How many different admins must approve opening a site to the network (no sign-in)."},
		{Name: "MAX_TEAMS_PER_PERSON", Group: "access", Type: "int", Default: itoa(int64(op.MaxTeamsPerPerson)), Min: 1, Max: maxTeamsPerPerson, Unit: "teams",
			Description: "Teams one person may belong to before they can create another."},
		{Name: "MAX_TEAM_MEMBERS", Group: "access", Type: "int", Default: itoa(int64(op.MaxTeamMembers)), Min: 1, Max: maxTeamMembers, Unit: "members",
			Description: "Members of one team, pending ones included."},
		{Name: "MAX_SITE_VIEWERS", Group: "access", Type: "int", Default: itoa(int64(op.MaxSiteViewers)), Min: 1, Max: maxSiteViewers, Unit: "entries",
			Description: "People and teams on one site's viewer list."},
		{Name: "ACCESS_LOG_VISIBILITY", Group: "access", Type: "enum", Default: defaultAccessLogVisibility, Allowed: []string{"counts", "owner", "admin"}, Security: true,
			Description: "What a site's owner sees of its visits: counts (views and distinct viewers), owner (each visit and who), or admin (nothing; admins only)."},

		// Sites and versions.
		{Name: "MAX_ARCHIVE_BYTES", Group: "sites", Type: "int", Default: itoa(op.MaxArchiveBytes), Min: minArchiveBytes, Max: maxArchiveBytes, Unit: "bytes",
			Description: "The largest archive one deploy may send. Raise the ingress body-size limit with it."},
		{Name: "MAX_FILES_PER_SITE", Group: "sites", Type: "int", Default: itoa(int64(op.MaxFilesPerSite)), Min: 1, Max: maxFilesPerSite, Unit: "files",
			Description: "Files one deploy may hold."},
		{Name: "UPLOAD_CONCURRENCY", Group: "sites", Type: "int", Default: itoa(int64(op.UploadConcurrency)), Min: 1, Max: maxUploadConcurrency, Unit: "uploads",
			Description: "Uploads (and, separately, downloads) one replica processes at once. Raise the pod's memory with it."},
		{Name: "QUOTA_MAX_SITES", Group: "sites", Type: "int", Default: itoa(defaultQuotaMaxSites), Min: 0, Unit: "sites",
			Description: "Sites one person or team may have. 0 is unlimited."},
		{Name: "QUOTA_MAX_BYTES", Group: "sites", Type: "int", Default: itoa(defaultQuotaMaxBytes), Min: 0, Unit: "bytes",
			Description: "Storage one person or team may use, every kept version and uploaded file included. 0 is unlimited."},
		{Name: "QUOTA_MAX_VERSIONS", Group: "sites", Type: "int", Default: itoa(defaultQuotaMaxVersions), Min: 1, Max: maxQuotaMaxVersions, Unit: "versions",
			Description: "Versions kept per site; older ones are removed after each deploy."},
		{Name: "PREVIEW_LINK_TTL", Group: "sites", Type: "duration", Default: d(op.PreviewLinkTTL), Min: d(minLinkTTL), Max: d(maxLinkTTL), Unit: "Go duration", Security: true,
			Description: "How long a preview link to a kept version works."},
		{Name: "EXPORT_LINK_TTL", Group: "sites", Type: "duration", Default: d(op.ExportLinkTTL), Min: d(minLinkTTL), Max: d(maxLinkTTL), Unit: "Go duration", Security: true,
			Description: "How long a whole-site download link works (once)."},
		{Name: "CLAMD_ADDR", Group: "sites", Type: "string", Security: true,
			Description: "A clamd scanner (host:port) that checks every uploaded file before it is stored. A scanner that is down refuses uploads."},
		{Name: "CLAMD_TIMEOUT", Group: "sites", Type: "duration", Default: d(defaultClamdTimeout), Unit: "Go duration",
			Description: "How long the scan of one file may take."},
		{Name: "ASSET_MAX_FILE_BYTES", Group: "sites", Type: "int", Default: itoa(defaultAssetMaxFileBytes), Min: 1, Unit: "bytes",
			Description: "The largest file a page may upload."},
		{Name: "ASSET_MAX_SITE_BYTES", Group: "sites", Type: "int", Default: itoa(defaultAssetMaxSiteBytes), Min: 1, Unit: "bytes",
			Description: "Uploaded files one site may hold, in bytes."},
		{Name: "ASSET_MAX_SITE_COUNT", Group: "sites", Type: "int", Default: itoa(defaultAssetMaxSiteCount), Min: 1, Unit: "files",
			Description: "Uploaded files one site may hold."},
		{Name: "SEARCH_SESSION_MAX_AGE", Group: "sites", Type: "duration", Default: d(op.SearchSessionMaxAge), Min: d(minSearchSessionMaxAge), Max: d(maxSearchSessionMaxAge), Unit: "Go duration",
			Description: "Lifetime of the anonymous cookie site search uses for its limits and counts."},

		// Cleanup and retention.
		{Name: "DELETED_RETENTION_DAYS", Group: "cleanup", Type: "int", Default: itoa(int64(op.DeletedRetentionDays)), Min: minDeletedRetentionDays, Max: maxDeletedRetentionDays, Unit: "days",
			Description: "How long a deleted site stays restorable in Recently deleted. Keep the bucket's old-version retention at least this long."},
		{Name: "IDLE_CLEANUP_DAYS", Group: "cleanup", Type: "int", Default: "0", Min: 0, Max: maxIdleCleanupDays, Unit: "days",
			Description: "Idle cleanup: a site nobody opened, deployed to or saved to for this many days is marked. 0 turns it off; otherwise at least 30."},
		{Name: "IDLE_CLEANUP_GRACE_DAYS", Group: "cleanup", Type: "int", Default: itoa(int64(op.IdleGraceDays)), Min: minIdleGraceDays, Max: maxIdleGraceDays, Unit: "days",
			Description: "Idle cleanup: how long a marked site waits before it moves to Recently deleted."},
		{Name: "IDLE_CLEANUP_MAX_EMAILS", Group: "cleanup", Type: "int", Default: itoa(int64(op.IdleMaxEmails)), Min: 0, Max: maxIdleMaxEmails, Unit: "emails",
			Description: "Idle cleanup: sites one hourly run marks and emails about. 0 is no limit."},
		{Name: "AUDIT_RETENTION_DAYS", Group: "cleanup", Type: "int", Default: itoa(defaultAuditRetentionDays), Min: 1, Unit: "days",
			Description: "How long the audit log is kept."},
		{Name: "ACCESS_LOG_RETENTION_DAYS", Group: "cleanup", Type: "int", Default: itoa(defaultAccessLogRetentionDays), Min: 1, Unit: "days",
			Description: "How long the access log, and ended sessions with their IP and browser, are kept."},
		{Name: "SEARCH_TELEMETRY_RETENTION_DAYS", Group: "cleanup", Type: "int", Default: itoa(int64(op.SearchTelemetryRetentionDays)), Min: 1, Max: maxSearchTelemetryRetention, Unit: "days",
			Description: "How long search queries and clicks are kept."},

		// Email.
		{Name: "SMTP_URL", Group: "email", Type: "secret", Basic: true, Security: true,
			Description: "Your mail relay, smtp://user:password@host:587 (STARTTLS) or smtps://host:465. It carries a password, so it goes in the Secret."},
		{Name: "SMTP_FROM", Group: "email", Type: "string", Basic: true,
			Description: "The sender, like Simple Host <hosting@example.com>. Required with SMTP_URL."},

		// Storage and backups.
		{Name: "DB_HOST", Group: "storage", Type: "string", Required: true, Basic: true,
			Description: "Your managed Postgres host. Or give DB_DSN instead of the parts."},
		{Name: "DB_PORT", Group: "storage", Type: "int", Default: defaultDBPort, Min: 1, Max: 65535,
			Description: "Its port."},
		{Name: "DB_NAME", Group: "storage", Type: "string", Required: true, Basic: true,
			Description: "The database name."},
		{Name: "DB_USER", Group: "storage", Type: "string", Required: true, Basic: true,
			Description: "The owning role the migrate and prune jobs connect as."},
		{Name: "DB_PASSWORD", Group: "storage", Type: "secret", Required: true, Basic: true, Security: true,
			Description: "The owning role's password. Never the same as DB_APP_PASSWORD."},
		{Name: "DB_APP_USER", Group: "storage", Type: "string",
			Description: "The least-privilege role the server connects as (the manifest sets simplehost_app)."},
		{Name: "DB_APP_PASSWORD", Group: "storage", Type: "secret", Required: true, Basic: true, Security: true,
			Description: "That role's password; migrate sets it on every run."},
		{Name: "DB_DSN", Group: "storage", Type: "secret", Security: true,
			Description: "A complete postgres:// URL, instead of the parts above."},
		{Name: "DB_SSLMODE", Group: "storage", Type: "string", Default: defaultDBSSLMode, Security: true,
			Description: "TLS to the database. Anything but verify-full needs DB_INSECURE_ALLOWED."},
		{Name: "DB_SSL_ROOT_CERT", Group: "storage", Type: "string", Security: true,
			Description: "The database's CA bundle, mounted from the simple-host-db-ca Secret."},
		{Name: "DB_INSECURE_ALLOWED", Group: "storage", Type: "bool", Default: "false", Allowed: []string{"true", "false"}, Security: true,
			Description: "Allows a database without verified TLS. Local evaluation only."},
		{Name: "DB_INCLUSTER_EVALUATION", Group: "storage", Type: "bool", Default: "false", Allowed: []string{"true", "false"}, Security: true,
			Description: "Set by the in-cluster evaluation Postgres, which nothing backs up; logs a warning at every start. Never set it on a real install: it marks the database as throwaway."},
		{Name: "BACKUP_STORAGE_ENDPOINT", Group: "storage", Type: "string", Required: true, Basic: true,
			Description: "The S3-compatible bucket's endpoint, over HTTPS."},
		{Name: "BACKUP_STORAGE_REGION", Group: "storage", Type: "string", Default: defaultBackupRegion, Basic: true,
			Description: "The bucket's region, where the provider needs one."},
		{Name: "BACKUP_STORAGE_BUCKET", Group: "storage", Type: "string", Required: true, Basic: true,
			Description: "The bucket that stores every site. Turn its versioning on."},
		{Name: "BACKUP_STORAGE_PREFIX", Group: "storage", Type: "string", Default: defaultBackupPrefix,
			Description: "The folder inside the bucket."},
		{Name: "BACKUP_STORAGE_ACCESS_KEY_ID", Group: "storage", Type: "secret", Basic: true, Security: true,
			Description: "The bucket's access key. Leave both keys out when a workload identity supplies credentials."},
		{Name: "BACKUP_STORAGE_SECRET_ACCESS_KEY", Group: "storage", Type: "secret", Basic: true, Security: true,
			Description: "The bucket's secret key."},
		{Name: "BACKUP_STORAGE_INSECURE_ALLOWED", Group: "storage", Type: "bool", Default: "false", Allowed: []string{"true", "false"}, Security: true,
			Description: "Allows a plain-HTTP bucket endpoint. Local evaluation only."},
		{Name: "BACKUP_SSE", Group: "storage", Type: "enum", Default: defaultBackupSSE, Allowed: []string{defaultBackupSSE, sseKMS}, Security: true,
			Description: "Server-side encryption on every stored object."},
		{Name: "BACKUP_SSE_KEY_ID", Group: "storage", Type: "string", Security: true,
			Description: "The KMS key, with BACKUP_SSE=aws:kms."},
		{Name: "BACKUP_ENVELOPE_KEY", Group: "storage", Type: "secret", Security: true,
			Description: "Optional encryption of every object before it leaves the pod: <id>:<base64 32-byte key> entries. Escrow it: sites are unreadable without it."},
		{Name: "BACKUP_ENVELOPE_PLAINTEXT_ALLOWED", Group: "storage", Type: "bool", Default: "false", Allowed: []string{"true", "false"}, Security: true,
			Description: "Keeps reading objects written before the envelope key was added, until reencrypt has covered them."},
		{Name: "CACHE_DIR", Group: "storage", Type: "string", Default: defaultCacheDir,
			Description: "The pod-local cache of site versions, emptied on start."},
		{Name: "CACHE_MAX_BYTES", Group: "storage", Type: "int", Default: itoa(defaultCacheMaxBytes), Min: 1, Unit: "bytes",
			Description: "Size of that cache. Size its volume at about three times this."},

		// Observability.
		{Name: "METRICS_PORT", Group: "observability", Type: "int", Default: defaultMetricsPort, Min: 1, Max: 65535,
			Description: "The port of the separate /metrics listener (Prometheus); not exposed by the Service or Ingress."},
	}
	for _, name := range RateLimitNames {
		doc := RateLimitDocs[name]
		all = append(all, Setting{Name: RateLimitEnv(name), Group: doc[0], Description: doc[1], Type: "rate"})
	}
	var out []Setting
	for _, g := range SettingGroups {
		for _, s := range all {
			if s.Group == g.ID {
				out = append(out, s)
			}
		}
	}
	if len(out) != len(all) {
		panic("config: a setting names a group that is not in SettingGroups")
	}
	return out
}
