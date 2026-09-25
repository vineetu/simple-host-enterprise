package mcp

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"unicode/utf8"
)

// maxArchiveReadBytes bounds a version archive held in memory to list or read
// it through the connector. The archive route allows two downloads at once,
// so this is also the bound on what the two can hold together, twice over.
const maxArchiveReadBytes = 64 << 20

// maxFileReadBytes bounds one file returned to a model. A larger file is
// named and refused rather than truncated: half a file read as the whole of
// it would be redeployed as the whole of it.
const maxFileReadBytes = 1 << 20

// errArchiveTooLarge is what a capped upstream body reports.
var errArchiveTooLarge = fmt.Errorf("this version is larger than %d MiB, too large to read through the connector; download it with GET /api/collaboration/sites/{owner}/{site}/versions/{version}/archive instead", maxArchiveReadBytes>>20)

func openArchive(archive []byte) (*zip.Reader, error) {
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("the version archive could not be read: %w", err)
	}
	return reader, nil
}

// listArchive answers list_site_files: every regular file, sorted by path.
func listArchive(archive []byte, version int) ([]byte, error) {
	reader, err := openArchive(archive)
	if err != nil {
		return nil, err
	}
	files := make([]map[string]any, 0, len(reader.File))
	for _, entry := range reader.File {
		if entry.FileInfo().IsDir() {
			continue
		}
		files = append(files, map[string]any{"path": entry.Name, "size": entry.UncompressedSize64})
	}
	sort.Slice(files, func(i, j int) bool { return files[i]["path"].(string) < files[j]["path"].(string) })
	return json.Marshal(map[string]any{"version": version, "count": len(files), "files": files})
}

// readArchiveFile answers read_site_file: one file, as text when it is valid
// UTF-8 and as base64 otherwise, so it can be sent back to deploy_site as is.
func readArchiveFile(archive []byte, name string, version int) ([]byte, error) {
	reader, err := openArchive(archive)
	if err != nil {
		return nil, err
	}
	for _, entry := range reader.File {
		if entry.Name != name || entry.FileInfo().IsDir() {
			continue
		}
		if entry.UncompressedSize64 > maxFileReadBytes {
			return nil, fmt.Errorf("%s is %d bytes, over the %d MiB a file can be read here; keep it unchanged by downloading the version archive instead", name, entry.UncompressedSize64, maxFileReadBytes>>20)
		}
		opened, err := entry.Open()
		if err != nil {
			return nil, fmt.Errorf("%s could not be read: %w", name, err)
		}
		content, err := io.ReadAll(io.LimitReader(opened, maxFileReadBytes+1))
		opened.Close()
		if err != nil {
			return nil, fmt.Errorf("%s could not be read: %w", name, err)
		}
		if len(content) > maxFileReadBytes {
			return nil, fmt.Errorf("%s is over the %d MiB a file can be read here", name, maxFileReadBytes>>20)
		}
		result := map[string]any{"version": version, "path": name, "size": len(content)}
		if utf8.Valid(content) {
			result["encoding"] = "text"
			result["content"] = string(content)
		} else {
			result["encoding"] = "base64"
			result["content"] = base64.StdEncoding.EncodeToString(content)
		}
		return json.Marshal(result)
	}
	return nil, fmt.Errorf("no file %q in version %d; call list_site_files for the exact paths", name, version)
}
