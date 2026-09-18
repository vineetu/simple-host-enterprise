package handler

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestCollaborationSkillReleaseIsPackaged(t *testing.T) {
	version, err := PluginVersion()
	if err != nil {
		t.Fatalf("PluginVersion: %v", err)
	}
	// Assert the invariant, not the number: the packaged skill must be stamped
	// with whatever plugin.json declares. Hardcoding it made every release break
	// the suite for no safety benefit.
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(version) {
		t.Fatalf("plugin version = %q, want major.minor.patch", version)
	}
	files := packagedSkillFiles(t)

	skill := files["simple-host/SKILL.md"]
	if !strings.Contains(skill, "This skill is version `"+version+"`") || strings.Contains(skill, "{{VERSION}}") {
		t.Fatal("packaged simple-host skill did not receive the release version")
	}
	if lines := strings.Count(skill, "\n") + 1; lines >= 500 {
		t.Fatalf("packaged core skill has %d lines, want fewer than 500", lines)
	}
	accessRule := strings.Index(skill, "Never use legacy `GET /api/sites` to determine editability")
	serviceSection := strings.Index(skill, "## Service")
	if accessRule < 0 || serviceSection < 0 || accessRule > serviceSection {
		t.Fatal("canonical access resolution is not a top-level early rule")
	}
	reference := files["simple-host/references/collaboration.md"]
	if !strings.Contains(reference, "If-Match") || !strings.Contains(reference, "/api/collaboration/sites") {
		t.Fatal("packaged skill is missing collaboration reference guidance")
	}
	for _, path := range []string{
		"simple-host/references/account-recovery.md",
		"simple-host/references/frameworks.md",
		"simple-host/references/packaging-and-validation.md",
		"simple-host/references/state-and-ai.md",
		"simple-host/references/updating.md",
	} {
		if strings.TrimSpace(files[path]) == "" {
			t.Fatalf("packaged skill is missing %s", path)
		}
	}
	updating := files["simple-host/references/updating.md"]
	for _, want := range []string{
		"Never update silently",   // no unattended instruction rewrites
		"Never downgrade",         // a newer local skill must win
		"Replace transactionally", // a failed update must not leave a mixed root
		"A failed update must leave the prior active\ninstallation intact.",
		"invoke the original task again", // reload, or hand back to the user
		"#simple-host-support",           // somewhere to go when it breaks
	} {
		if !strings.Contains(updating, want) {
			t.Errorf("packaged update reference is missing %q", want)
		}
	}
	if len(updating) > 6000 {
		t.Errorf("update reference is %d characters; it was cut from 14480 and must not creep back", len(updating))
	}
}

func TestAccountRecoveryTransactionIsPackaged(t *testing.T) {
	reference := packagedSkillFiles(t)["simple-host/references/account-recovery.md"]
	assertSkillSubstringsInOrder(t, reference,
		"Never use the workspace or another writable directory as a credential fallback",
		// Identity is sign-in by a human, not a call the agent can make
		// itself — the whole point of the OIDC-backed flow.
		"Identity is sign-in, not self-registration.",
		"## Existing config",
		// The sign-in link comes before every stop condition, because handing
		// over one URL is what replaced "explain the failure and give up".
		"## Get a key",
		"not send the user to Slack",
		"<base>/auth/login",
		"Wait for the user to paste back the key",
		"## Prove the exact destination first",
		"## Persist and verify",
		"same-directory temporary config",
		"Use only that disk-loaded key for `GET /api/me`",
		"must never appear in command arguments, environment variables, stdout,",
		"## A team is not an account",
		"## Revoked, lost, or extra keys",
		"There is no email-based reset path",
		"Never hot-loop, expose a credential, or ask the user to disclose a password",
	)
}

func packagedSkillFiles(t *testing.T) map[string]string {
	t.Helper()

	bundle, err := buildSkillsZip()
	if err != nil {
		t.Fatalf("buildSkillsZip: %v", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(bundle), int64(len(bundle)))
	if err != nil {
		t.Fatalf("open skills zip: %v", err)
	}
	files := make(map[string]string, len(reader.File))
	for _, entry := range reader.File {
		opened, err := entry.Open()
		if err != nil {
			t.Fatalf("open %s: %v", entry.Name, err)
		}
		body, readErr := io.ReadAll(opened)
		closeErr := opened.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read %s: read=%v close=%v", entry.Name, readErr, closeErr)
		}
		files[entry.Name] = string(body)
	}
	return files
}

func assertSkillSubstringsInOrder(t *testing.T, value string, markers ...string) {
	t.Helper()
	position := 0
	for _, marker := range markers {
		next := strings.Index(value[position:], marker)
		if next < 0 {
			t.Fatalf("text after byte %d is missing ordered marker %q", position, marker)
		}
		position += next + len(marker)
	}
}

func TestSkillsVersionManifestMatchesServedBundle(t *testing.T) {
	recorder := httptest.NewRecorder()
	serveSkillsVersion(recorder, httptest.NewRequest(http.MethodGet, "/skills/version", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}

	var manifest skillVersionManifest
	if err := json.Unmarshal(recorder.Body.Bytes(), &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	zipRecorder := httptest.NewRecorder()
	serveSkillsZip(zipRecorder, httptest.NewRequest(http.MethodGet, "/skills.zip", nil))
	if zipRecorder.Code != http.StatusOK {
		t.Fatalf("skills.zip status = %d, want 200", zipRecorder.Code)
	}
	bundle := zipRecorder.Body.Bytes()
	sum := sha256.Sum256(bundle)

	if manifest.ManifestVersion != 1 || manifest.BundleName != "simple-host-skills" {
		t.Errorf("manifest identity = version %d bundle %q", manifest.ManifestVersion, manifest.BundleName)
	}
	pluginVersion, err := PluginVersion()
	if err != nil {
		t.Fatalf("PluginVersion: %v", err)
	}
	if manifest.Version != pluginVersion || manifest.MinimumSupportedVersion != MinimumSupportedSkillVersion {
		t.Errorf("version range = %q..%q, want %q..%q", manifest.MinimumSupportedVersion, manifest.Version, MinimumSupportedSkillVersion, pluginVersion)
	}
	if manifest.BundleURL != "/skills.zip" || manifest.BundleSize != len(bundle) {
		t.Errorf("bundle metadata = URL %q size %d; want /skills.zip size %d", manifest.BundleURL, manifest.BundleSize, len(bundle))
	}
	if manifest.SHA256 != fmt.Sprintf("%x", sum) {
		t.Errorf("sha256 = %q, want %x", manifest.SHA256, sum)
	}
	// The release type is checked against the constant rather than a literal.
	// Pinning the word here meant every release bump failed a test that was
	// not actually testing anything about the release — what matters is that
	// the manifest serves what the server declares, and that the word is one
	// an agent knows how to act on.
	switch skillReleaseType {
	case "patch", "minor", "major":
	default:
		t.Errorf("skillReleaseType = %q, want patch, minor or major", skillReleaseType)
	}
	if manifest.ReleaseType != skillReleaseType || !manifest.RequiresReload || manifest.ReleaseNotesURL != "/changelog.html" {
		t.Errorf("release metadata = type %q reload %t notes %q; want type %q", manifest.ReleaseType, manifest.RequiresReload, manifest.ReleaseNotesURL, skillReleaseType)
	}

	// Preserve the two legacy fields consumed by pre-manifest installers.
	var legacy map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &legacy); err != nil {
		t.Fatalf("decode legacy view: %v", err)
	}
	if legacy["version"] == nil || legacy["sha256"] == nil {
		t.Fatal("manifest dropped legacy version or sha256 field")
	}
}

// Every skill at or below 0.8.1 requires bundle_url to equal "/skills.zip" and
// downloads from that fixed path. Repointing it at the content-addressed route
// would make each of those installs reject its own update and strand itself.
func TestMutableBundleContractSurvivesForInstalledSkills(t *testing.T) {
	recorder := httptest.NewRecorder()
	serveSkillsVersion(recorder, httptest.NewRequest(http.MethodGet, "/skills/version", nil))

	var manifest skillVersionManifest
	if err := json.Unmarshal(recorder.Body.Bytes(), &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if manifest.BundleURL != "/skills.zip" {
		t.Fatalf("bundle_url = %q, want /skills.zip — installed skills validate this exact value", manifest.BundleURL)
	}

	legacyRecorder := httptest.NewRecorder()
	serveSkillsZip(legacyRecorder, httptest.NewRequest(http.MethodGet, "/skills.zip", nil))
	if legacyRecorder.Code != http.StatusOK {
		t.Fatalf("/skills.zip status = %d, want 200", legacyRecorder.Code)
	}
}

// The bundle URL is addressed by digest rather than by version because go:embed
// fixes bytes per binary, not per version number: editing a SKILL.md without
// bumping plugin.json yields different bytes under an unchanged version. A
// version-addressed URL would then serve different content while promising
// immutability. This walks the path a real updater takes — read the manifest,
// follow its immutable_bundle_url, verify size and digest.
func TestImmutableBundleIsContentAddressedAndFollowable(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /skills.zip", serveSkillsZip)
	mux.HandleFunc("GET /skills/version", serveSkillsVersion)
	mux.HandleFunc("GET /skills/sha256/{digest}/skills.zip", serveImmutableSkillsZip)

	manifestRecorder := httptest.NewRecorder()
	mux.ServeHTTP(manifestRecorder, httptest.NewRequest(http.MethodGet, "/skills/version", nil))
	var manifest skillVersionManifest
	if err := json.Unmarshal(manifestRecorder.Body.Bytes(), &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}

	// The URL must name the manifest's own digest, so it cannot address bytes
	// other than the ones the manifest just described.
	if want := "/skills/sha256/" + manifest.SHA256 + "/skills.zip"; manifest.ImmutableBundleURL != want {
		t.Fatalf("immutable_bundle_url = %q, want %q", manifest.ImmutableBundleURL, want)
	}

	followed := httptest.NewRecorder()
	mux.ServeHTTP(followed, httptest.NewRequest(http.MethodGet, manifest.ImmutableBundleURL, nil))
	if followed.Code != http.StatusOK {
		t.Fatalf("following immutable_bundle_url = %d, want 200", followed.Code)
	}
	body := followed.Body.Bytes()
	if len(body) != manifest.BundleSize {
		t.Errorf("followed bundle size = %d, want %d", len(body), manifest.BundleSize)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(body)); got != manifest.SHA256 {
		t.Errorf("followed bundle digest = %s, want %s", got, manifest.SHA256)
	}
	if got := followed.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q, want an immutable directive", got)
	}

	legacy := httptest.NewRecorder()
	mux.ServeHTTP(legacy, httptest.NewRequest(http.MethodGet, "/skills.zip", nil))
	if !bytes.Equal(body, legacy.Body.Bytes()) {
		t.Error("content-addressed bundle differs from /skills.zip")
	}

	// A digest we do not serve must 404, and that 404 must not be cached: it is
	// only correct for the currently deployed bundle.
	for name, digest := range map[string]string{
		"unknown digest": strings.Repeat("a", 64),
		"empty payload":  fmt.Sprintf("%x", sha256.Sum256(nil)),
		"not hex":        "latest",
	} {
		miss := httptest.NewRecorder()
		mux.ServeHTTP(miss, httptest.NewRequest(http.MethodGet, "/skills/sha256/"+digest+"/skills.zip", nil))
		if miss.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", name, miss.Code)
		}
		if got := miss.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s Cache-Control = %q, want no-store", name, got)
		}
	}
}

func TestOpenAPIDocumentsEveryManagementClientBootstrap(t *testing.T) {
	body, err := fs.ReadFile(staticFiles, "static/openapi.yaml")
	if err != nil {
		t.Fatalf("read embedded OpenAPI: %v", err)
	}
	document := string(body)

	for _, want := range []string{
		"version: 2.5.0",
		"/skills/version:",
		"SkillVersionManifest:",
		"X-Simple-Host-Client",
		"skill_version_required",
		"X-Simple-Host-Latest-Skill-Version",
	} {
		if !strings.Contains(document, want) {
			t.Errorf("OpenAPI is missing %q", want)
		}
	}
	if got := strings.Count(document, "#/components/parameters/SkillVersion"); got != 18 {
		t.Fatalf("documented %d guarded management operations, want 18", got)
	}
	if got := strings.Count(document, "#/components/parameters/SimpleHostClient"); got != 18 {
		t.Fatalf("documented %d direct-client classifications, want 18", got)
	}
	if got := strings.Count(document, "#/components/responses/SkillVersionRequired") +
		strings.Count(document, "#/components/responses/ManagementBadRequest"); got != 18 {
		t.Fatalf("documented %d management bootstrap errors, want 18", got)
	}
}
