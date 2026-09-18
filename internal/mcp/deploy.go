package mcp

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"path"
	"strings"
)

// maxInlineBytes caps a deploy assembled from inline files. The REST archive
// upload accepts far more; this path exists so an agent can deploy without
// producing a tarball, and every byte travels inside a JSON-RPC message, so it
// is deliberately bounded well below the REST limit. Most sites are far smaller
// than this — the median live site is under a megabyte.
const maxInlineBytes = 8 << 20

// buildArchive turns inline files into the same tar.gz the REST endpoint takes,
// so both paths land in one extractor with one set of guards.
func buildArchive(args map[string]any) ([]byte, error) {
	raw, present := args["files"]
	if !present {
		return nil, fmt.Errorf("files is required: the complete list of files the site consists of")
	}
	rawFiles, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("files must be an array of {path, content} objects, got %T", raw)
	}
	if len(rawFiles) == 0 {
		return nil, fmt.Errorf("files must not be empty; a deploy needs at least an index.html")
	}

	var buffer bytes.Buffer
	zip := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(zip)

	total := 0
	seen := make(map[string]bool, len(rawFiles))
	sawIndex := false

	for index, raw := range rawFiles {
		entry, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("files[%d] must be an object", index)
		}
		name, err := stringArg(entry, "path")
		if err != nil {
			return nil, fmt.Errorf("files[%d]: %w", index, err)
		}
		clean, err := cleanRelPath(name)
		if err != nil {
			return nil, fmt.Errorf("files[%d]: %w", index, err)
		}
		if seen[clean] {
			return nil, fmt.Errorf("files[%d]: %s appears more than once", index, clean)
		}
		seen[clean] = true
		if clean == "index.html" {
			sawIndex = true
		}

		content, present := entry["content"]
		if !present {
			return nil, fmt.Errorf("files[%d]: content is required", index)
		}
		text, ok := content.(string)
		if !ok {
			return nil, fmt.Errorf("files[%d]: content must be a string", index)
		}

		var payload []byte
		encoding, err := optionalString(entry, "encoding")
		if err != nil {
			return nil, fmt.Errorf("files[%d]: %w", index, err)
		}
		switch strings.ToLower(encoding) {
		case "", "text":
			payload = []byte(text)
		case "base64":
			payload, err = base64.StdEncoding.DecodeString(text)
			if err != nil {
				return nil, fmt.Errorf("files[%d]: content is not valid base64", index)
			}
		default:
			return nil, fmt.Errorf("files[%d]: encoding must be text or base64", index)
		}

		total += len(payload)
		if total > maxInlineBytes {
			return nil, fmt.Errorf("files exceed the %d MiB inline limit; use the REST archive upload for a site this size", maxInlineBytes>>20)
		}

		header := &tar.Header{
			Name:     clean,
			Mode:     0o644,
			Size:     int64(len(payload)),
			Typeflag: tar.TypeReg,
		}
		if err := archive.WriteHeader(header); err != nil {
			return nil, fmt.Errorf("write %s: %w", clean, err)
		}
		if _, err := archive.Write(payload); err != nil {
			return nil, fmt.Errorf("write %s: %w", clean, err)
		}
	}

	if !sawIndex {
		return nil, fmt.Errorf("no index.html at the site root; the site would not serve")
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	if err := zip.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// cleanRelPath rejects anything that would escape the site root. The extractor
// enforces this again on the server side; catching it here turns a rejected
// upload into a precise message the model can act on.
func cleanRelPath(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", fmt.Errorf("path must not be empty")
	}
	if strings.HasPrefix(trimmed, "/") {
		return "", fmt.Errorf("path must be relative, got %q", name)
	}
	if strings.ContainsRune(trimmed, '\\') {
		return "", fmt.Errorf("path must use forward slashes, got %q", name)
	}
	clean := path.Clean(trimmed)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path escapes the site root: %q", name)
	}
	return clean, nil
}
