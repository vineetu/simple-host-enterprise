package tarball

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"fmt"
	"hash/crc32"
	"runtime"
	"strings"
	"testing"
)

// TestExtractRefusesOversizedTarDeclaredSizeWithBoundedMemory exercises the
// archive-bomb OOM fix directly: a tar entry whose header already declares
// more than the per-file cap must be refused before its body is read, not
// after a full allocation discovers the overage (which is what let
// TestArchiveBombs' "a single file over the 500 MiB per-file limit" case
// OOM-kill the pod -- see docs/security-review.md). The
// entry's real payload is 600 MiB of zero bytes, comfortably over the 500
// MiB per-file limit, but because it is one repeated byte gzip compresses it
// down to a few KB, so this test exercises a genuine 600 MiB decompressed
// stream without needing a 600 MiB fixture or a 600 MiB test heap.
func TestExtractRefusesOversizedTarDeclaredSizeWithBoundedMemory(t *testing.T) {
	const declaredSize = 600 * 1024 * 1024 // over defaultExtractLimits.file (500 MiB)
	archive := buildZeroFilledTarGz(t, "big.bin", declaredSize)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := ExtractBytes(archive, "site.tar.gz")
	runtime.ReadMemStats(&after)

	if err == nil {
		t.Fatal("entry over the per-file cap unexpectedly accepted")
	}
	if !strings.Contains(err.Error(), "exceeding the") {
		t.Fatalf("unexpected error: %v", err)
	}

	// TotalAlloc is a monotonic counter of bytes ever allocated, unaffected by
	// GC timing, so this is a direct assertion that refusing the entry never
	// allocated anywhere near its declared (or actual) size.
	const allocBudget = 16 * 1024 * 1024 // far below the 600 MiB declared size
	if delta := after.TotalAlloc - before.TotalAlloc; delta > allocBudget {
		t.Fatalf("refusing an oversized entry allocated %d bytes, want under %d", delta, allocBudget)
	}
}

func buildZeroFilledTarGz(t *testing.T, name string, size int64) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: size, Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	chunk := make([]byte, 4*1024*1024)
	var written int64
	for written < size {
		n := int64(len(chunk))
		if remaining := size - written; remaining < n {
			n = remaining
		}
		if _, err := tw.Write(chunk[:n]); err != nil {
			t.Fatalf("Write: %v", err)
		}
		written += n
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// TestExtractRefusesZipEntryWhoseDeclaredSizeLies covers the direction a
// declared size can lie that checkDeclaredSize's pre-read check cannot catch
// on its own: a zip entry's central-directory UncompressedSize64 can
// understate what its deflate stream actually produces, since deflate is
// self-terminating and does not stop at any declared length. This exercises
// readFileContent's own defense -- the one extra byte probed for after the
// declared boundary -- catching the lie for the cost of one byte rather than
// the size of the hidden payload.
func TestExtractRefusesZipEntryWhoseDeclaredSizeLies(t *testing.T) {
	const declaredSize = 10
	const actualSize = 4096
	archive := buildZipWithLyingSize(t, "payload.bin", declaredSize, actualSize)

	_, err := ExtractBytes(archive, "site.zip")
	if err == nil {
		t.Fatal("entry with a lying declared size unexpectedly accepted")
	}
	// The probe read past the declared boundary is what surfaces this: either
	// readFileContent's own "produced more" check catches the extra byte, or
	// (as observed with Go's current archive/zip) the stdlib's own checksum
	// reader notices the actual stream disagrees with the declared size first
	// and returns its own zip.ErrFormat. Either is a correct refusal; the
	// probe read is what makes either check happen at all, since without it
	// nothing past the declared boundary is ever touched.
	if !strings.Contains(err.Error(), "produced more") && !strings.Contains(err.Error(), "not a valid zip file") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func buildZipWithLyingSize(t *testing.T, name string, declaredSize, actualSize int) []byte {
	t.Helper()
	payload := bytes.Repeat([]byte{'z'}, actualSize)

	var compressed bytes.Buffer
	fw, err := flate.NewWriter(&compressed, flate.BestSpeed)
	if err != nil {
		t.Fatalf("flate.NewWriter: %v", err)
	}
	if _, err := fw.Write(payload); err != nil {
		t.Fatalf("flate write: %v", err)
	}
	if err := fw.Close(); err != nil {
		t.Fatalf("flate close: %v", err)
	}

	var buffer bytes.Buffer
	zw := zip.NewWriter(&buffer)
	header := &zip.FileHeader{
		Name:               name,
		Method:             zip.Deflate,
		CRC32:              crc32.ChecksumIEEE(payload),
		UncompressedSize64: uint64(declaredSize), // lies: the real content is actualSize bytes
		CompressedSize64:   uint64(compressed.Len()),
	}
	rawWriter, err := zw.CreateRaw(header)
	if err != nil {
		t.Fatalf("CreateRaw: %v", err)
	}
	if _, err := rawWriter.Write(compressed.Bytes()); err != nil {
		t.Fatalf("write raw entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// TestExtractRefusesEntryCountOverDefaultLimit exercises the real,
// documented 50,000-entry cap (not a scaled-down test limit): 50,001 entries
// must be refused, and refused before the cost of extracting any of their
// content.
func TestExtractRefusesEntryCountOverDefaultLimit(t *testing.T) {
	for _, format := range []string{"tar.gz", "zip"} {
		format := format
		t.Run(format, func(t *testing.T) {
			entries := make([]testEntry, maxArchiveEntries+1)
			for i := range entries {
				entries[i] = testEntry{name: fmt.Sprintf("f%05d", i)}
			}
			archive := buildArchive(t, format, entries)

			_, err := ExtractBytes(archive, "site."+format)
			if err == nil {
				t.Fatal("archive over the default entry limit unexpectedly accepted")
			}
			if !strings.Contains(err.Error(), "entry limit") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestExtractAggregateCapAcrossManyFiles proves the aggregate uncompressed
// budget is enforced by accumulation across many entries, not just a
// two-file case: the individual files here each fit comfortably under the
// per-file cap, and only their sum crosses the aggregate one.
func TestExtractAggregateCapAcrossManyFiles(t *testing.T) {
	for _, format := range []string{"tar.gz", "zip"} {
		format := format
		t.Run(format, func(t *testing.T) {
			const perFile = 100
			const fileCount = 20
			entries := make([]testEntry, fileCount)
			for i := range entries {
				entries[i] = testEntry{name: fmt.Sprintf("f%02d.bin", i), data: bytes.Repeat([]byte{'a'}, perFile)}
			}
			archive := buildArchive(t, format, entries)

			limits := defaultExtractLimits
			limits.file = perFile
			limits.total = perFile*fileCount - 1 // one byte short of the true aggregate sum

			_, err := extractWithLimits(bytes.NewReader(archive), "site."+format, limits)
			if err == nil {
				t.Fatal("aggregate cap across many files was not enforced")
			}
			// The last file's own declared size no longer fits what remains of
			// the aggregate budget, so checkDeclaredSize refuses it (a more
			// precise, earlier refusal than discovering the aggregate total
			// itself went negative) -- either message proves the accumulation
			// across many files is what triggered it.
			if !strings.Contains(err.Error(), "uncompressed size limit") && !strings.Contains(err.Error(), "byte limit") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
