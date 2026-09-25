package handler

import (
	"archive/zip"
	"bytes"
	"io/fs"
	"net/http"
	"path"
	"strings"

	plugin "github.com/vsriram/simple-host/simple-host-plugin"
)

// RegisterPluginRoute serves GET /plugin.zip: an installable plugin for this
// instance — the Claude plugin (.claude-plugin/plugin.json, .mcp.json) and
// the Agent Plugins manifest (plugin.json, mcp.json) side by side, with the
// skills — its MCP server already pointing at <base>/mcp.
func RegisterPluginRoute(mux *http.ServeMux, publicBaseURL string) {
	baseURL := strings.TrimRight(publicBaseURL, "/")
	mux.HandleFunc("GET /plugin.zip", func(w http.ResponseWriter, r *http.Request) {
		data, err := buildPluginZip(baseURL)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="simple-host-plugin.zip"`)
		http.ServeContent(w, r, "simple-host-plugin.zip", skillsModTime, bytes.NewReader(data))
	})
}

// fillTemplate replaces the placeholders a plugin or skill file may carry.
func fillTemplate(body, version, baseURL string) string {
	return strings.NewReplacer("{{VERSION}}", version, "{{BASE_URL}}", baseURL).Replace(body)
}

func buildPluginZip(baseURL string) ([]byte, error) {
	version, err := PluginVersion()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	err = fs.WalkDir(plugin.FS, ".", func(name string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		body, err := fs.ReadFile(plugin.FS, name)
		if err != nil {
			return err
		}
		if ext := path.Ext(name); ext == ".md" || ext == ".json" {
			body = []byte(fillTemplate(string(body), version, baseURL))
		}
		out, err := zw.Create("simple-host/" + name)
		if err != nil {
			return err
		}
		_, err = out.Write(body)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
