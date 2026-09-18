package handler

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	plugin "github.com/vsriram/simple-host/simple-host-plugin"
)

func TestAgentSkillsDiscoveryIndexAndArchives(t *testing.T) {
	mux := http.NewServeMux()
	RegisterUIRoutes(mux)

	indexResponse := serveUIRequest(t, mux, http.MethodGet, agentSkillsDiscoveryRoot+"index.json")
	if indexResponse.Code != http.StatusOK {
		t.Fatalf("index status = %d, want 200; body=%s", indexResponse.Code, indexResponse.Body.String())
	}
	assertDiscoveryHeaders(t, indexResponse, "application/json")

	var rawIndex map[string]json.RawMessage
	if err := json.Unmarshal(indexResponse.Body.Bytes(), &rawIndex); err != nil {
		t.Fatalf("decode raw index: %v", err)
	}
	if len(rawIndex) != 2 || rawIndex["$schema"] == nil || rawIndex["skills"] == nil {
		t.Fatalf("index top-level fields = %v, want exactly $schema and skills", reflect.ValueOf(rawIndex).MapKeys())
	}

	var index agentSkillsDiscoveryIndex
	if err := json.Unmarshal(indexResponse.Body.Bytes(), &index); err != nil {
		t.Fatalf("decode index: %v", err)
	}
	if index.Schema != agentSkillsDiscoverySchema {
		t.Fatalf("$schema = %q, want %q", index.Schema, agentSkillsDiscoverySchema)
	}
	if len(index.Skills) != len(agentSkillsDiscoveryAllowlist) {
		t.Fatalf("skill count = %d, want %d", len(index.Skills), len(agentSkillsDiscoveryAllowlist))
	}

	var rawEntries []map[string]json.RawMessage
	if err := json.Unmarshal(rawIndex["skills"], &rawEntries); err != nil {
		t.Fatalf("decode raw skill entries: %v", err)
	}
	version, err := PluginVersion()
	if err != nil {
		t.Fatalf("PluginVersion: %v", err)
	}

	for i, entry := range index.Skills {
		wantName := agentSkillsDiscoveryAllowlist[i]
		if entry.Name != wantName {
			t.Errorf("skill[%d].name = %q, want %q", i, entry.Name, wantName)
		}
		if len(rawEntries[i]) != 5 {
			t.Errorf("skill[%d] has %d fields, want exactly 5", i, len(rawEntries[i]))
		}
		for _, field := range []string{"name", "description", "type", "url", "digest"} {
			if rawEntries[i][field] == nil {
				t.Errorf("skill[%d] is missing %q", i, field)
			}
		}

		wantDescription := embeddedSkillDescriptionForTest(t, wantName)
		if entry.Description != wantDescription {
			t.Errorf("skill[%d].description = %q, want embedded frontmatter description %q", i, entry.Description, wantDescription)
		}
		if entry.Type != "archive" {
			t.Errorf("skill[%d].type = %q, want archive", i, entry.Type)
		}
		wantURL := agentSkillsDiscoveryRoot + wantName + ".zip"
		if entry.URL != wantURL || !strings.HasPrefix(entry.URL, "/") || strings.HasPrefix(entry.URL, "//") {
			t.Errorf("skill[%d].url = %q, want path-absolute URL %q", i, entry.URL, wantURL)
		}

		digestHex, ok := strings.CutPrefix(entry.Digest, "sha256:")
		if !ok || len(digestHex) != sha256.Size*2 {
			t.Errorf("skill[%d].digest = %q, want sha256:<64 lowercase hex>", i, entry.Digest)
		} else if decoded, err := hex.DecodeString(digestHex); err != nil || hex.EncodeToString(decoded) != digestHex {
			t.Errorf("skill[%d].digest = %q, want lowercase hexadecimal: %v", i, entry.Digest, err)
		}

		archiveResponse := serveUIRequest(t, mux, http.MethodGet, entry.URL)
		if archiveResponse.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200; body=%s", entry.URL, archiveResponse.Code, archiveResponse.Body.String())
		}
		assertDiscoveryHeaders(t, archiveResponse, "application/zip")
		archive := archiveResponse.Body.Bytes()
		sum := sha256.Sum256(archive)
		if got := "sha256:" + hex.EncodeToString(sum[:]); entry.Digest != got {
			t.Errorf("skill[%d].digest = %q, want exact served archive digest %q", i, entry.Digest, got)
		}

		secondArchive, err := buildAgentSkillArchive(wantName)
		if err != nil {
			t.Fatalf("rebuild %s archive: %v", wantName, err)
		}
		if !bytes.Equal(archive, secondArchive) {
			t.Errorf("%s archive bytes are not deterministic", wantName)
		}
		assertDiscoveryArchive(t, wantName, archive, version)
	}
}

func TestAgentSkillsDiscoveryGETHEADParity(t *testing.T) {
	mux := http.NewServeMux()
	RegisterUIRoutes(mux)

	paths := []string{agentSkillsDiscoveryRoot + "index.json"}
	for _, skillName := range agentSkillsDiscoveryAllowlist {
		paths = append(paths, agentSkillsDiscoveryRoot+skillName+".zip")
	}

	for _, requestPath := range paths {
		t.Run(requestPath, func(t *testing.T) {
			getResponse := serveUIRequest(t, mux, http.MethodGet, requestPath)
			headResponse := serveUIRequest(t, mux, http.MethodHead, requestPath)

			if getResponse.Code != http.StatusOK || headResponse.Code != getResponse.Code {
				t.Fatalf("GET status = %d, HEAD status = %d, want matching 200", getResponse.Code, headResponse.Code)
			}
			if !reflect.DeepEqual(getResponse.Header(), headResponse.Header()) {
				t.Errorf("GET headers = %#v, HEAD headers = %#v", getResponse.Header(), headResponse.Header())
			}
			if headResponse.Body.Len() != 0 {
				t.Errorf("HEAD body length = %d, want 0", headResponse.Body.Len())
			}
			if getResponse.Body.Len() == 0 {
				t.Error("GET body is empty")
			}
			if got := getResponse.Header().Get("Content-Length"); got != strconv.Itoa(getResponse.Body.Len()) {
				t.Errorf("Content-Length = %q, want %d", got, getResponse.Body.Len())
			}
		})
	}
}

func TestAgentSkillsDiscoveryRejectsInvalidPathsAndMethods(t *testing.T) {
	mux := http.NewServeMux()
	RegisterUIRoutes(mux)

	for _, requestPath := range []string{
		agentSkillsDiscoveryRoot + "unknown.zip",
		agentSkillsDiscoveryRoot + "simple-host.zip/extra",
		agentSkillsDiscoveryRoot + "simple-host/SKILL.md",
	} {
		response := serveUIRequest(t, mux, http.MethodGet, requestPath)
		if response.Code != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", requestPath, response.Code)
		}
	}

	for _, requestPath := range []string{
		agentSkillsDiscoveryRoot + "index.json",
		agentSkillsDiscoveryRoot + "simple-host.zip",
	} {
		response := serveUIRequest(t, mux, http.MethodPost, requestPath)
		if response.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s status = %d, want 405", requestPath, response.Code)
		}
	}

	for _, skillName := range []string{"", "unknown", "../simple-host", "simple-host/SKILL.md"} {
		if _, err := buildAgentSkillArchive(skillName); err == nil {
			t.Errorf("buildAgentSkillArchive(%q) succeeded outside the fixed allowlist", skillName)
		}
	}
}

func TestAgentSkillsDiscoveryPreservesLegacyRoutes(t *testing.T) {
	mux := http.NewServeMux()
	RegisterUIRoutes(mux)

	versionResponse := serveUIRequest(t, mux, http.MethodGet, "/skills/version")
	if versionResponse.Code != http.StatusOK {
		t.Fatalf("GET /skills/version status = %d, want 200; body=%s", versionResponse.Code, versionResponse.Body.String())
	}
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(versionResponse.Body.Bytes(), &manifest); err != nil {
		t.Fatalf("decode /skills/version: %v", err)
	}
	if manifest["version"] == nil || manifest["sha256"] == nil {
		t.Fatal("/skills/version dropped legacy version or sha256")
	}

	bundleResponse := serveUIRequest(t, mux, http.MethodGet, "/skills.zip")
	if bundleResponse.Code != http.StatusOK {
		t.Fatalf("GET /skills.zip status = %d, want 200; body=%s", bundleResponse.Code, bundleResponse.Body.String())
	}
	bundle, err := zip.NewReader(bytes.NewReader(bundleResponse.Body.Bytes()), int64(bundleResponse.Body.Len()))
	if err != nil {
		t.Fatalf("open legacy /skills.zip: %v", err)
	}
	foundLegacyRoot := false
	for _, file := range bundle.File {
		if file.Name == "simple-host/SKILL.md" {
			foundLegacyRoot = true
			break
		}
	}
	if !foundLegacyRoot {
		t.Fatal("legacy /skills.zip no longer contains simple-host/SKILL.md")
	}

	rootResponse := serveUIRequest(t, mux, http.MethodGet, "/")
	if rootResponse.Code != http.StatusOK || rootResponse.Body.Len() == 0 {
		t.Errorf("GET / status = %d with %d-byte body, want 200 and static content", rootResponse.Code, rootResponse.Body.Len())
	}
}

func serveUIRequest(t *testing.T, mux *http.ServeMux, method, requestPath string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(method, requestPath, nil))
	return response
}

func assertDiscoveryHeaders(t *testing.T, response *httptest.ResponseRecorder, contentType string) {
	t.Helper()
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := response.Header().Get("Content-Type"); got != contentType {
		t.Errorf("Content-Type = %q, want %q", got, contentType)
	}
	if got := response.Header().Get("Content-Length"); got != strconv.Itoa(response.Body.Len()) {
		t.Errorf("Content-Length = %q, want %d", got, response.Body.Len())
	}
}

func embeddedSkillDescriptionForTest(t *testing.T, skillName string) string {
	t.Helper()
	body, err := fs.ReadFile(plugin.FS, "skills/"+skillName+"/SKILL.md")
	if err != nil {
		t.Fatalf("read embedded %s/SKILL.md: %v", skillName, err)
	}
	lines := strings.Split(string(body), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		t.Fatalf("embedded %s/SKILL.md has no frontmatter", skillName)
	}
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "---" {
			break
		}
		if description, ok := strings.CutPrefix(line, "description: "); ok {
			return strings.TrimSpace(description)
		}
	}
	t.Fatalf("embedded %s/SKILL.md has no description", skillName)
	return ""
}

func assertDiscoveryArchive(t *testing.T, skillName string, archive []byte, version string) {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("open %s archive: %v", skillName, err)
	}

	skillFS, err := fs.Sub(plugin.FS, "skills/"+skillName)
	if err != nil {
		t.Fatalf("open embedded %s skill: %v", skillName, err)
	}
	expected := make(map[string][]byte)
	if err := fs.WalkDir(skillFS, ".", func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filePath == "." || entry.IsDir() {
			return nil
		}
		body, err := fs.ReadFile(skillFS, filePath)
		if err != nil {
			return err
		}
		if strings.HasSuffix(filePath, "SKILL.md") {
			body = []byte(strings.ReplaceAll(string(body), "{{VERSION}}", version))
		}
		expected[filePath] = body
		return nil
	}); err != nil {
		t.Fatalf("walk embedded %s skill: %v", skillName, err)
	}

	if len(reader.File) != len(expected) {
		t.Fatalf("%s archive has %d files, want %d", skillName, len(reader.File), len(expected))
	}
	names := make([]string, 0, len(reader.File))
	var rootSkill []byte
	for _, file := range reader.File {
		names = append(names, file.Name)
		if file.Name == "" || strings.HasPrefix(file.Name, "/") || strings.Contains(file.Name, "\\") || path.Clean(file.Name) != file.Name {
			t.Errorf("%s archive contains unsafe path %q", skillName, file.Name)
		}
		if strings.HasPrefix(file.Name, skillName+"/") {
			t.Errorf("%s archive retained a top-level skill directory in %q", skillName, file.Name)
		}
		if !file.FileInfo().Mode().IsRegular() {
			t.Errorf("%s archive entry %q is not a regular file", skillName, file.Name)
		}

		want, ok := expected[file.Name]
		if !ok {
			t.Errorf("%s archive contains unexpected file %q", skillName, file.Name)
			continue
		}
		opened, err := file.Open()
		if err != nil {
			t.Fatalf("open %s archive entry %q: %v", skillName, file.Name, err)
		}
		got, readErr := io.ReadAll(opened)
		closeErr := opened.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read %s archive entry %q: read=%v close=%v", skillName, file.Name, readErr, closeErr)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s archive entry %q differs from embedded source after version replacement", skillName, file.Name)
		}
		if file.Name == "SKILL.md" {
			rootSkill = got
		}
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("%s archive paths are not deterministic lexical order: %v", skillName, names)
	}
	if len(rootSkill) == 0 {
		t.Fatalf("%s archive has no root SKILL.md", skillName)
	}
	if strings.Contains(string(rootSkill), "{{VERSION}}") {
		t.Errorf("%s root SKILL.md contains unresolved version placeholder", skillName)
	}
	sourceSkill, err := fs.ReadFile(skillFS, "SKILL.md")
	if err != nil {
		t.Fatalf("read embedded %s/SKILL.md: %v", skillName, err)
	}
	if strings.Contains(string(sourceSkill), "{{VERSION}}") && !strings.Contains(string(rootSkill), version) {
		t.Errorf("%s root SKILL.md does not contain concrete version %q", skillName, version)
	}
}
