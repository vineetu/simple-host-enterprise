package handler

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	plugin "github.com/vsriram/simple-host/simple-host-plugin"
)

//go:embed all:static
var staticFiles embed.FS

// skillsModTime is set at process start. Updated whenever a build is
// rebuilt; static for cache headers across the pod's lifetime.
var skillsModTime = time.Now().UTC()

const (
	skillManifestVersion = 1
	skillBundleName      = "simple-host-skills"
	skillBundleURL       = "/skills.zip"
	// skillReleaseType is hand-set, so it has to move with the version above
	// it. 0.12.0 documents API key scopes (publish/full/offboard), upload
	// quotas and two-admin network approval; 0.11.0 stays the minimum
	// (notice_middleware.go), since no route an 0.11.0 skill calls went
	// away. An agent reads this to decide how loudly to mention the update.
	skillReleaseType     = "minor"
	skillReleaseNotesURL = "/changelog.html"

	agentSkillsDiscoverySchema = "https://schemas.agentskills.io/discovery/0.2.0/schema.json"
	agentSkillsDiscoveryRoot   = "/.well-known/agent-skills/"
)

var agentSkillsDiscoveryAllowlist = [...]string{
	"simple-host",
	"simple-host-builder",
	"fix-paths-for-subpath-hosting",
}

type skillVersionManifest struct {
	ManifestVersion         int    `json:"manifest_version"`
	BundleName              string `json:"bundle_name"`
	Version                 string `json:"version"`
	MinimumSupportedVersion string `json:"minimum_supported_version"`
	BundleURL               string `json:"bundle_url"`
	ImmutableBundleURL      string `json:"immutable_bundle_url"`
	SHA256                  string `json:"sha256"`
	BundleSize              int    `json:"bundle_size"`
	ReleaseType             string `json:"release_type"`
	RequiresReload          bool   `json:"requires_reload"`
	ReleaseNotesURL         string `json:"release_notes_url"`
}

type agentSkillsDiscoveryIndex struct {
	Schema string                     `json:"$schema"`
	Skills []agentSkillDiscoveryEntry `json:"skills"`
}

type agentSkillDiscoveryEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"`
	URL         string `json:"url"`
	Digest      string `json:"digest"`
}

// PluginVersion reads .claude-plugin/plugin.json from the embedded plugin
// FS and returns its version string. Cheap; the embed FS lives in memory.
func PluginVersion() (string, error) {
	body, err := fs.ReadFile(plugin.FS, ".claude-plugin/plugin.json")
	if err != nil {
		return "", err
	}
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		return "", err
	}
	return manifest.Version, nil
}

// SkillBaseURLPlaceholder is the text a served skill file uses for this
// installation's own origin. Every Markdown file in a served bundle has it
// replaced with PUBLIC_BASE_URL by ExpandSkillText, so the package ships no
// hostname of its own. Anything else that serves the skill files (a plugin
// zip, for one) must expand them the same way.
const SkillBaseURLPlaceholder = "{{BASE_URL}}"

// skillVersionPlaceholder is replaced in SKILL.md files only, as it always
// has been: a reference may name the placeholder literally when telling an
// agent what an unexpanded bundle looks like.
const skillVersionPlaceholder = "{{VERSION}}"

// ExpandSkillText fills the placeholders in one served skill file. baseURL
// must be the validated PUBLIC_BASE_URL; an empty one is refused rather than
// serving instructions that point nowhere.
func ExpandSkillText(path string, body []byte, version, baseURL string) ([]byte, error) {
	if !strings.HasSuffix(strings.ToLower(path), ".md") {
		return body, nil
	}
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("skill %s: no public base URL to expand %s with", path, SkillBaseURLPlaceholder)
	}
	text := strings.ReplaceAll(string(body), SkillBaseURLPlaceholder, strings.TrimRight(baseURL, "/"))
	if strings.HasSuffix(path, "SKILL.md") {
		text = strings.ReplaceAll(text, skillVersionPlaceholder, version)
	}
	return []byte(text), nil
}

// RegisterUIRoutes serves the landing pages and the skill bundle. baseURL is
// PUBLIC_BASE_URL, written into every served skill file.
func RegisterUIRoutes(mux *http.ServeMux, baseURL string) {
	sub, _ := fs.Sub(staticFiles, "static")
	fileServer := http.FileServerFS(sub)

	mux.HandleFunc("GET "+agentSkillsDiscoveryRoot+"index.json", serveAgentSkillsDiscoveryIndex(baseURL))
	mux.HandleFunc("HEAD "+agentSkillsDiscoveryRoot+"index.json", serveAgentSkillsDiscoveryIndex(baseURL))
	for _, skillName := range agentSkillsDiscoveryAllowlist {
		archivePath := agentSkillsDiscoveryRoot + skillName + ".zip"
		handler := serveAgentSkillArchive(skillName, baseURL)
		mux.HandleFunc("GET "+archivePath, handler)
		mux.HandleFunc("HEAD "+archivePath, handler)
	}

	mux.HandleFunc("GET /skills.zip", serveSkillsZip(baseURL))
	mux.HandleFunc("GET /skills/version", serveSkillsVersion(baseURL))
	mux.HandleFunc("GET /skills/sha256/{digest}/skills.zip", serveImmutableSkillsZip(baseURL))
	mux.Handle("GET /", fileServer)
}

func serveAgentSkillsDiscoveryIndex(baseURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")

		data, err := buildAgentSkillsDiscoveryIndex(baseURL)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}

		serveAgentSkillsRepresentation(w, r, "application/json", data)
	}
}

func serveAgentSkillArchive(skillName, baseURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")

		data, err := buildAgentSkillArchive(skillName, baseURL)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}

		serveAgentSkillsRepresentation(w, r, "application/zip", data)
	}
}

func serveAgentSkillsRepresentation(w http.ResponseWriter, r *http.Request, contentType string, data []byte) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}

func buildAgentSkillsDiscoveryIndex(baseURL string) ([]byte, error) {
	entries := make([]agentSkillDiscoveryEntry, 0, len(agentSkillsDiscoveryAllowlist))
	for _, skillName := range agentSkillsDiscoveryAllowlist {
		description, err := agentSkillDescription(skillName)
		if err != nil {
			return nil, err
		}
		archive, err := buildAgentSkillArchive(skillName, baseURL)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(archive)
		entries = append(entries, agentSkillDiscoveryEntry{
			Name:        skillName,
			Description: description,
			Type:        "archive",
			URL:         agentSkillsDiscoveryRoot + skillName + ".zip",
			Digest:      fmt.Sprintf("sha256:%x", sum),
		})
	}

	return json.Marshal(agentSkillsDiscoveryIndex{
		Schema: agentSkillsDiscoverySchema,
		Skills: entries,
	})
}

func agentSkillDescription(skillName string) (string, error) {
	body, err := fs.ReadFile(plugin.FS, "skills/"+skillName+"/SKILL.md")
	if err != nil {
		return "", err
	}

	lines := strings.Split(string(body), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", fmt.Errorf("skill %q has no frontmatter", skillName)
	}
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "---" {
			break
		}
		if description, ok := strings.CutPrefix(line, "description: "); ok {
			description = strings.TrimSpace(description)
			if description == "" {
				break
			}
			return description, nil
		}
	}
	return "", fmt.Errorf("skill %q has no frontmatter description", skillName)
}

func buildAgentSkillArchive(skillName, baseURL string) ([]byte, error) {
	if !isAgentSkillDiscoverable(skillName) {
		return nil, fmt.Errorf("skill %q is not discoverable", skillName)
	}

	version, err := PluginVersion()
	if err != nil {
		return nil, err
	}
	skillFS, err := fs.Sub(plugin.FS, "skills/"+skillName)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	err = fs.WalkDir(skillFS, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == "." || d.IsDir() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("skill %q contains non-regular file %q", skillName, path)
		}
		return writeZipEntry(zw, skillFS, path, version, baseURL)
	})
	if err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func isAgentSkillDiscoverable(skillName string) bool {
	for _, allowed := range agentSkillsDiscoveryAllowlist {
		if skillName == allowed {
			return true
		}
	}
	return false
}

func serveSkillsVersion(baseURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeSkillsVersion(w, baseURL)
	}
}

func writeSkillsVersion(w http.ResponseWriter, baseURL string) {
	version, err := PluginVersion()
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	// sha256 of the exact bytes /skills.zip serves. Computed from the same
	// buildSkillsZip() so the two endpoints are provably equal (the builder is
	// deterministic for a fixed binary). The agent verifies the downloaded zip
	// against this before extracting — an integrity checksum, not a signature.
	// Do NOT cache: the per-request rebuild is what keeps them in lockstep.
	data, err := buildSkillsZip(baseURL)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	sum := sha256.Sum256(data)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(skillVersionManifest{
		ManifestVersion:         skillManifestVersion,
		BundleName:              skillBundleName,
		Version:                 version,
		MinimumSupportedVersion: MinimumSupportedSkillVersion,
		BundleURL:               skillBundleURL,
		ImmutableBundleURL:      immutableBundleURL(fmt.Sprintf("%x", sum)),
		SHA256:                  fmt.Sprintf("%x", sum),
		BundleSize:              len(data),
		ReleaseType:             skillReleaseType,
		RequiresReload:          true,
		ReleaseNotesURL:         skillReleaseNotesURL,
	})
}

// immutableBundleURL returns the content-addressed path for a bundle.
//
// This is deliberately keyed on the digest rather than the version. go:embed
// fixes the bytes for one *binary*, not for a version number across binaries:
// editing a SKILL.md without bumping plugin.json produces different bytes under
// an unchanged version, so a version-addressed URL would quietly start serving
// different content after a redeploy. A digest-addressed URL cannot do that —
// different bytes are a different URL by construction, which is the only way
// the immutable Cache-Control below is truthful. See RFC 8246.
func immutableBundleURL(digest string) string {
	return "/skills/sha256/" + digest + "/skills.zip"
}

// serveImmutableSkillsZip serves the bundle only when the requested digest
// matches the bundle this binary produces. Anything else is a 404 rather than
// a silent fallback to different bytes.
//
// The mutable /skills.zip stays permanently: every skill at or below 0.8.1
// hardcodes it and validates bundle_url == "/skills.zip". Removing it, or
// pointing bundle_url here, would strand every existing install.
func serveImmutableSkillsZip(baseURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		immutableSkillsZip(w, r, baseURL)
	}
}

func immutableSkillsZip(w http.ResponseWriter, r *http.Request, baseURL string) {
	data, err := buildSkillsZip(baseURL)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	if r.PathValue("digest") != fmt.Sprintf("%x", sha256.Sum256(data)) {
		// 404s are heuristically cacheable, and this one is only correct for
		// the currently deployed bundle, so forbid storing it.
		w.Header().Set("Cache-Control", "no-store")
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="simple-host-skills.zip"`)
	// Truthful here, and only here: the URL names the exact bytes.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, "simple-host-skills.zip", skillsModTime, bytes.NewReader(data))
}

// serveSkillsZip returns a flat zip of the three skill folders, suitable for
// extraction directly into ~/.claude/skills or ~/.agents/skills.
func serveSkillsZip(baseURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := buildSkillsZip(baseURL)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="simple-host-skills.zip"`)
		http.ServeContent(w, r, "simple-host-skills.zip", skillsModTime, bytes.NewReader(data))
	}
}

// buildSkillsZip walks the embedded skills/ tree and emits a flat zip.
// Rebuilds on every call (no sync.Once cache) so SKILL.md edits picked up
// via hot-deploy serve immediately. Cheap — the embedded FS lives in RAM
// and the result is a few hundred KB.
//
// Every file passes through ExpandSkillText: SKILL.md gets the current
// plugin.json version, so the client-side version check and X-Skill-Version
// header stay in lockstep with the server, and every Markdown file gets this
// installation's origin.
func buildSkillsZip(baseURL string) ([]byte, error) {
	version, err := PluginVersion()
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	skillsRoot, err := fs.Sub(plugin.FS, "skills")
	if err != nil {
		return nil, err
	}

	err = fs.WalkDir(skillsRoot, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == "." || d.IsDir() {
			return nil
		}

		if err := writeZipEntry(zw, skillsRoot, path, version, baseURL); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeZipEntry copies one file from src into zw, expanded by
// ExpandSkillText.
func writeZipEntry(zw *zip.Writer, src fs.FS, path, version, baseURL string) error {
	body, err := fs.ReadFile(src, path)
	if err != nil {
		return err
	}
	body, err = ExpandSkillText(path, body, version, baseURL)
	if err != nil {
		return err
	}
	out, err := zw.Create(path)
	if err != nil {
		return err
	}
	_, err = out.Write(body)
	return err
}
