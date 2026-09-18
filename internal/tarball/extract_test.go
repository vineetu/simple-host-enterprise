package tarball

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"strings"
	"testing"
)

type testEntry struct {
	name     string
	data     []byte
	mode     os.FileMode
	typeflag byte
}

func TestExtractValidArchives(t *testing.T) {
	entries := []testEntry{
		{name: "./", mode: os.ModeDir | 0o755},
		{name: "./assets/", mode: os.ModeDir | 0o755},
		{name: "./index.html", data: []byte("hello")},
		{name: "assets/download.dmg", data: []byte("dmg bytes")},
		{name: "assets/bundle.zip", data: []byte("zip download bytes")},
		{name: "assets/.DS_Store", data: []byte("ignored")},
	}

	for _, format := range []string{"tar.gz", "zip"} {
		format := format
		t.Run(format, func(t *testing.T) {
			archive := buildArchive(t, format, entries)
			files, err := ExtractBytes(archive, "site."+format)
			if err != nil {
				t.Fatalf("ExtractBytes: %v", err)
			}
			if got := string(files["index.html"]); got != "hello" {
				t.Fatalf("index.html = %q", got)
			}
			if got := string(files["assets/download.dmg"]); got != "dmg bytes" {
				t.Fatalf("download.dmg = %q", got)
			}
			if _, exists := files["assets/.DS_Store"]; exists {
				t.Fatal(".DS_Store should be omitted")
			}
		})
	}
}

func TestExtractRejectsUnsafePaths(t *testing.T) {
	unsafe := []string{
		"../escape",
		"/absolute",
		`windows\path`,
		"a//b",
		"a/./b",
		"a/../b",
		"a\nb",
	}
	for _, format := range []string{"tar.gz", "zip"} {
		format := format
		formatUnsafe := append([]string(nil), unsafe...)
		// archive/tar refuses to encode these names before extraction; ZIP can
		// carry them and therefore exercises the extractor boundary directly.
		if format == "zip" {
			formatUnsafe = append(formatUnsafe, "a\x00b", string([]byte{0xff}))
		}
		for _, name := range formatUnsafe {
			name := name
			t.Run(format+"_unsafe", func(t *testing.T) {
				archive := buildArchive(t, format, []testEntry{{name: name, data: []byte("x")}})
				if _, err := ExtractBytes(archive, "site."+format); err == nil {
					t.Fatalf("unsafe path %q unexpectedly accepted", name)
				}
			})
		}
	}
}

func TestExtractRejectsDuplicatesAndPrefixConflicts(t *testing.T) {
	tests := []struct {
		name    string
		entries []testEntry
	}{
		{name: "normalized duplicate", entries: []testEntry{{name: "index.html", data: []byte("a")}, {name: "./index.html", data: []byte("b")}}},
		{name: "file then child", entries: []testEntry{{name: "a", data: []byte("a")}, {name: "a/b", data: []byte("b")}}},
		{name: "child then file", entries: []testEntry{{name: "a/b", data: []byte("b")}, {name: "a", data: []byte("a")}}},
		{name: "directory then same file", entries: []testEntry{{name: "a/", mode: os.ModeDir | 0o755}, {name: "a", data: []byte("a")}}},
	}
	for _, format := range []string{"tar.gz", "zip"} {
		format := format
		for _, test := range tests {
			test := test
			t.Run(format+"_"+test.name, func(t *testing.T) {
				archive := buildArchive(t, format, test.entries)
				if _, err := ExtractBytes(archive, "site."+format); err == nil {
					t.Fatal("conflicting paths unexpectedly accepted")
				}
			})
		}
	}
}

func TestExtractRejectsLinksAndSpecialEntries(t *testing.T) {
	tarArchive := buildArchive(t, "tar.gz", []testEntry{{name: "link", data: []byte("target"), typeflag: tar.TypeSymlink}})
	if _, err := ExtractBytes(tarArchive, "site.tar.gz"); err == nil {
		t.Fatal("tar symlink unexpectedly accepted")
	}

	zipArchive := buildArchive(t, "zip", []testEntry{{name: "link", data: []byte("target"), mode: os.ModeSymlink | 0o777}})
	if _, err := ExtractBytes(zipArchive, "site.zip"); err == nil {
		t.Fatal("zip symlink unexpectedly accepted")
	}
}

func TestExtractEnforcesActualByteAndEntryLimits(t *testing.T) {
	for _, format := range []string{"tar.gz", "zip"} {
		format := format
		t.Run(format+"_file", func(t *testing.T) {
			archive := buildArchive(t, format, []testEntry{{name: "large.bin", data: []byte("12345")}})
			limits := defaultExtractLimits
			limits.file = 4
			if _, err := extractWithLimits(bytes.NewReader(archive), "site."+format, limits); err == nil {
				t.Fatal("per-file byte limit was not enforced")
			}
		})
		t.Run(format+"_total", func(t *testing.T) {
			archive := buildArchive(t, format, []testEntry{{name: "a.bin", data: []byte("123")}, {name: "b.bin", data: []byte("456")}})
			limits := defaultExtractLimits
			limits.total = 5
			limits.file = 10
			if _, err := extractWithLimits(bytes.NewReader(archive), "site."+format, limits); err == nil {
				t.Fatal("aggregate byte limit was not enforced")
			}
		})
		t.Run(format+"_skipped_total", func(t *testing.T) {
			archive := buildArchive(t, format, []testEntry{{name: ".DS_Store", data: []byte("123456")}})
			limits := defaultExtractLimits
			limits.total = 5
			limits.file = 10
			if _, err := extractWithLimits(bytes.NewReader(archive), "site."+format, limits); err == nil {
				t.Fatal("skipped file bypassed aggregate byte limit")
			}
		})
		t.Run(format+"_entry_count", func(t *testing.T) {
			archive := buildArchive(t, format, []testEntry{
				{name: "dir/", mode: os.ModeDir | 0o755},
				{name: ".DS_Store", data: []byte("x")},
				{name: "index.html", data: []byte("x")},
			})
			limits := defaultExtractLimits
			limits.entries = 2
			if _, err := extractWithLimits(bytes.NewReader(archive), "site."+format, limits); err == nil {
				t.Fatal("directory/skipped entries bypassed entry limit")
			}
		})
	}
}

func TestExtractRejectsDirectoryPayload(t *testing.T) {
	// Standard tar/ZIP writers refuse nonzero directory payloads. Build a ZIP
	// regular file, then replace its same-length local and central names so the
	// reader observes a directory entry carrying data.
	archive := buildArchive(t, "zip", []testEntry{{name: "dirx", data: []byte("payload")}})
	archive = bytes.ReplaceAll(archive, []byte("dirx"), []byte("dir/"))
	if _, err := ExtractBytes(archive, "site.zip"); err == nil {
		t.Fatal("zip directory payload unexpectedly accepted")
	}
}

func TestExtractCompressedLimit(t *testing.T) {
	limits := defaultExtractLimits
	limits.compressed = 3
	if _, err := extractWithLimits(strings.NewReader("1234"), "site.zip", limits); err == nil {
		t.Fatal("compressed byte limit was not enforced")
	}
}

func TestExtractEnforcesPathMetadataBudget(t *testing.T) {
	components := make([]string, 0, 32)
	for index := 0; index < 31; index++ {
		components = append(components, strings.Repeat("d", 31))
	}
	components = append(components, strings.Repeat("f", 31))
	name := strings.Join(components, "/")
	if len(name) != 1023 {
		t.Fatalf("adversarial path length = %d, want 1023", len(name))
	}

	for _, format := range []string{"tar.gz", "zip"} {
		t.Run(format, func(t *testing.T) {
			archive := buildArchive(t, format, []testEntry{{name: name, data: []byte("x")}})
			limits := defaultExtractLimits
			// The explicit path alone fits. Its depth-32 implicit ancestors do
			// not, proving this guard is independent of the one-entry archive
			// and one-byte payload limits exercised here.
			limits.pathMetadata = int64(len(name)) + pathMetadataEntryOverhead
			limits.entries = 2
			limits.total = 2
			limits.file = 2

			_, err := extractWithLimits(bytes.NewReader(archive), "site."+format, limits)
			if err == nil || !strings.Contains(err.Error(), "path metadata exceeds") {
				t.Fatalf("path metadata error = %v", err)
			}
		})
	}
}

func TestExtractTarGzEnforcesPhysicalStreamLimitInsidePAXChain(t *testing.T) {
	archive := buildPAXFloodTarGz(t, 8, 700)
	files, err := ExtractBytes(archive, "site.tar.gz")
	if err != nil {
		t.Fatalf("valid PAX fixture: %v", err)
	}
	if got := string(files["index.html"]); got != "x" {
		t.Fatalf("valid PAX fixture index.html = %q", got)
	}

	limits := defaultExtractLimits
	limits.tarStream = 4 * 1024
	limits.entries = 2
	limits.total = 2
	limits.file = 2

	_, err = extractWithLimits(bytes.NewReader(archive), "site.tar.gz", limits)
	if err == nil || !strings.Contains(err.Error(), "decompressed tar stream exceeds") {
		t.Fatalf("PAX physical stream error = %v", err)
	}
}

func TestExtractTarGzEnforcesPhysicalStreamLimitAfterLogicalEOF(t *testing.T) {
	logicalArchive := buildTarGz(t, []testEntry{{name: "index.html", data: []byte("x")}})
	logicalStream := gunzipBytes(t, logicalArchive)
	bombMember := gzipBytes(t, bytes.Repeat([]byte("z"), 4096))
	archive := append(append([]byte(nil), logicalArchive...), bombMember...)

	limits := defaultExtractLimits
	limits.tarStream = int64(len(logicalStream) + 1024)
	limits.entries = 2
	limits.total = 2
	limits.file = 2

	_, err := extractWithLimits(bytes.NewReader(archive), "site.tar.gz", limits)
	if err == nil || !strings.Contains(err.Error(), "decompressed tar stream exceeds") {
		t.Fatalf("physical stream error = %v", err)
	}
}

func TestExtractTarGzAllowsPhysicalStreamExactlyAtLimit(t *testing.T) {
	archive := buildTarGz(t, []testEntry{{name: "index.html", data: []byte("x")}})
	physicalStream := gunzipBytes(t, archive)
	limits := defaultExtractLimits
	limits.tarStream = int64(len(physicalStream))

	files, err := extractWithLimits(bytes.NewReader(archive), "site.tar.gz", limits)
	if err != nil {
		t.Fatalf("exact physical stream limit: %v", err)
	}
	if got := string(files["index.html"]); got != "x" {
		t.Fatalf("index.html = %q", got)
	}
}

func buildArchive(t *testing.T, format string, entries []testEntry) []byte {
	t.Helper()
	if format == "tar.gz" {
		return buildTarGz(t, entries)
	}
	return buildZip(t, entries)
}

func buildTarGz(t *testing.T, entries []testEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			if entry.mode.IsDir() {
				typeflag = tar.TypeDir
			} else {
				typeflag = tar.TypeReg
			}
		}
		header := &tar.Header{Name: entry.name, Mode: 0o644, Size: int64(len(entry.data)), Typeflag: typeflag}
		if entry.mode.IsDir() {
			header.Mode = 0o755
		}
		if typeflag != tar.TypeReg && typeflag != tar.TypeRegA && typeflag != tar.TypeDir {
			header.Size = 0
			header.Linkname = string(entry.data)
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("WriteHeader(%q): %v", entry.name, err)
		}
		if header.Size > 0 {
			if _, err := tw.Write(entry.data); err != nil {
				t.Fatalf("Write(%q): %v", entry.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func buildPAXFloodTarGz(t *testing.T, headerCount, valueBytes int) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	header := &tar.Header{
		Name:       "index.html",
		Mode:       0o644,
		Size:       1,
		Typeflag:   tar.TypeReg,
		Format:     tar.FormatPAX,
		PAXRecords: map[string]string{"comment": strings.Repeat("x", valueBytes)},
	}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	// archive/tar deliberately refuses callers manually writing TypeXHeader.
	// Let it encode one valid local PAX header, find the real regular-file
	// header, then repeat the complete encoded PAX header+payload prefix. A TAR
	// reader must consume every chained prefix internally before returning the
	// logical index.html entry.
	actualHeaderOffset := -1
	for offset := 0; offset+512 <= raw.Len(); offset += 512 {
		block := raw.Bytes()[offset : offset+512]
		name := string(bytes.TrimRight(block[:100], "\x00"))
		if name == "index.html" && (block[156] == 0 || block[156] == tar.TypeReg || block[156] == tar.TypeRegA) {
			actualHeaderOffset = offset
			break
		}
	}
	if actualHeaderOffset <= 0 {
		t.Fatalf("could not locate regular header after encoded PAX metadata")
	}
	metadata := raw.Bytes()[:actualHeaderOffset]
	physical := append(bytes.Repeat(metadata, headerCount), raw.Bytes()[actualHeaderOffset:]...)
	return gzipBytes(t, physical)
}

func gunzipBytes(t *testing.T, archive []byte) []byte {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	return content
}

func gzipBytes(t *testing.T, content []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func buildZip(t *testing.T, entries []testEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	zw := zip.NewWriter(&buffer)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		if entry.mode != 0 {
			header.SetMode(entry.mode)
		}
		writer, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatalf("CreateHeader(%q): %v", entry.name, err)
		}
		if _, err := io.Copy(writer, bytes.NewReader(entry.data)); err != nil {
			t.Fatalf("Write(%q): %v", entry.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
