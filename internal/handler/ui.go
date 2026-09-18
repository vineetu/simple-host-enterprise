package handler

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"fmt"
	"io"
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
	// it. 0.9.0 adds teams: new tools, a new reference, and a changed deploy
	// contract, which is a minor release rather than a patch. An agent reads
	// this to decide how loudly to mention the update.
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

func RegisterUIRoutes(mux *http.ServeMux) {
	sub, _ := fs.Sub(staticFiles, "static")
	fileServer := http.FileServerFS(sub)

	mux.HandleFunc("GET "+agentSkillsDiscoveryRoot+"index.json", serveAgentSkillsDiscoveryIndex)
	mux.HandleFunc("HEAD "+agentSkillsDiscoveryRoot+"index.json", serveAgentSkillsDiscoveryIndex)
	for _, skillName := range agentSkillsDiscoveryAllowlist {
		archivePath := agentSkillsDiscoveryRoot + skillName + ".zip"
		handler := serveAgentSkillArchive(skillName)
		mux.HandleFunc("GET "+archivePath, handler)
		mux.HandleFunc("HEAD "+archivePath, handler)
	}

	mux.HandleFunc("GET /skills.zip", serveSkillsZip)
	mux.HandleFunc("GET /skills/version", serveSkillsVersion)
	mux.HandleFunc("GET /skills/sha256/{digest}/skills.zip", serveImmutableSkillsZip)
	mux.Handle("GET /", fileServer)
}

func serveAgentSkillsDiscoveryIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	data, err := buildAgentSkillsDiscoveryIndex()
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	serveAgentSkillsRepresentation(w, r, "application/json", data)
}

func serveAgentSkillArchive(skillName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")

		data, err := buildAgentSkillArchive(skillName)
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

func buildAgentSkillsDiscoveryIndex() ([]byte, error) {
	entries := make([]agentSkillDiscoveryEntry, 0, len(agentSkillsDiscoveryAllowlist))
	for _, skillName := range agentSkillsDiscoveryAllowlist {
		description, err := agentSkillDescription(skillName)
		if err != nil {
			return nil, err
		}
		archive, err := buildAgentSkillArchive(skillName)
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

func buildAgentSkillArchive(skillName string) ([]byte, error) {
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
		return writeZipEntry(zw, skillFS, path, version)
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

func serveSkillsVersion(w http.ResponseWriter, r *http.Request) {
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
	// See docs/secure-skill-install/design.md.
	data, err := buildSkillsZip()
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
func serveImmutableSkillsZip(w http.ResponseWriter, r *http.Request) {
	data, err := buildSkillsZip()
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
func serveSkillsZip(w http.ResponseWriter, r *http.Request) {
	data, err := buildSkillsZip()
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="simple-host-skills.zip"`)
	http.ServeContent(w, r, "simple-host-skills.zip", skillsModTime, bytes.NewReader(data))
}

// buildSkillsZip walks the embedded skills/ tree and emits a flat zip.
// Rebuilds on every call (no sync.Once cache) so SKILL.md edits picked up
// via hot-deploy serve immediately. Cheap — the embedded FS lives in RAM
// and the result is a few hundred KB.
//
// SKILL.md content has {{VERSION}} placeholders replaced with the current
// plugin.json version so the client-side version check and X-Skill-Version
// header stay in lockstep with the server without any human discipline.
func buildSkillsZip() ([]byte, error) {
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

		if err := writeZipEntry(zw, skillsRoot, path, version); err != nil {
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

// writeZipEntry copies one file from src into zw. For SKILL.md files, the
// content is read into memory and {{VERSION}} placeholders are replaced
// with the current plugin version before writing. Other files stream
// through unchanged.
func writeZipEntry(zw *zip.Writer, src fs.FS, path, version string) error {
	in, err := src.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := zw.Create(path)
	if err != nil {
		return err
	}

	if strings.HasSuffix(path, "SKILL.md") {
		body, err := io.ReadAll(in)
		if err != nil {
			return err
		}
		_, err = out.Write([]byte(strings.ReplaceAll(string(body), "{{VERSION}}", version)))
		return err
	}

	_, err = io.Copy(out, in)
	return err
}
