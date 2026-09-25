package tarball

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/vsriram/simple-host/internal/safepath"
)

const (
	maxCompressedSize        int64 = 100 * 1024 * 1024
	maxTotalUncompressedSize int64 = 500 * 1024 * 1024
	maxFileUncompressedSize  int64 = 500 * 1024 * 1024
	// TAR framing, padding, extended headers, and bytes after the logical TAR
	// terminator are not part of the extracted-file total. Bound the complete
	// gzip output independently so those bytes cannot become a decompression
	// bomb.
	maxTarStreamSize int64 = 768 * 1024 * 1024
	// A canonical archive entry can create an implicit directory for every
	// path component. Budget the unique map keys (including a conservative
	// per-entry allowance for Go map/string bookkeeping) independently of the
	// explicit archive-header limit.
	maxPathMetadataSize       int64 = 64 * 1024 * 1024
	pathMetadataEntryOverhead int64 = 64
	maxArchiveEntries               = 50_000
)

type extractLimits struct {
	compressed   int64
	total        int64
	file         int64
	tarStream    int64
	pathMetadata int64
	entries      int
}

var defaultExtractLimits = extractLimits{
	compressed:   maxCompressedSize,
	total:        maxTotalUncompressedSize,
	file:         maxFileUncompressedSize,
	tarStream:    maxTarStreamSize,
	pathMetadata: maxPathMetadataSize,
	entries:      maxArchiveEntries,
}

func Extract(r io.Reader, filename string) (map[string][]byte, error) {
	return extractWithLimits(r, filename, defaultExtractLimits)
}

// ExtractBytes avoids copying a request body that the caller has already
// bounded and buffered.
func ExtractBytes(archiveBytes []byte, filename string) (map[string][]byte, error) {
	if int64(len(archiveBytes)) > defaultExtractLimits.compressed {
		return nil, fmt.Errorf("read archive: content exceeds %d byte limit", defaultExtractLimits.compressed)
	}
	return extractArchiveBytes(archiveBytes, filename, defaultExtractLimits)
}

func extractWithLimits(r io.Reader, filename string, limits extractLimits) (map[string][]byte, error) {
	archiveBytes, err := readAtMost(r, limits.compressed)
	if err != nil {
		return nil, fmt.Errorf("read archive: %w", err)
	}
	return extractArchiveBytes(archiveBytes, filename, limits)
}

func extractArchiveBytes(archiveBytes []byte, filename string, limits extractLimits) (map[string][]byte, error) {
	switch {
	case strings.HasSuffix(strings.ToLower(filename), ".tar.gz"):
		return extractTarGz(bytes.NewReader(archiveBytes), limits)
	case strings.HasSuffix(strings.ToLower(filename), ".zip"):
		return extractZip(archiveBytes, limits)
	default:
		return nil, fmt.Errorf("unsupported archive format: %s", path.Ext(filename))
	}
}

func extractTarGz(r io.Reader, limits extractLimits) (map[string][]byte, error) {
	if limits.tarStream < 0 || limits.tarStream == int64(^uint64(0)>>1) {
		return nil, fmt.Errorf("invalid decompressed tar stream limit")
	}
	gzipReader, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("create gzip reader: %w", err)
	}
	defer gzipReader.Close()

	// Keep one sentinel byte beyond the accepted boundary. At logical TAR EOF
	// we drain this reader through gzip EOF, which includes TAR padding,
	// trailing bytes, and concatenated gzip members and also validates the gzip
	// checksum. A plain tar.Reader would otherwise stop at the TAR zero blocks.
	physicalStream := &io.LimitedReader{R: gzipReader, N: limits.tarStream + 1}
	tarReader := tar.NewReader(physicalStream)
	files := make(map[string][]byte)
	paths := newArchivePathTracker(limits.pathMetadata)
	var totalSize int64
	entryCount := 0

	for {
		header, err := tarReader.Next()
		if tarStreamExceeded(physicalStream) {
			return nil, tarStreamLimitError(limits.tarStream)
		}
		if err == io.EOF {
			if _, drainErr := io.Copy(io.Discard, physicalStream); drainErr != nil {
				return nil, fmt.Errorf("read decompressed tar stream: %w", drainErr)
			}
			if tarStreamExceeded(physicalStream) {
				return nil, tarStreamLimitError(limits.tarStream)
			}
			return files, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read tar entry: %w", err)
		}
		entryCount++
		if entryCount > limits.entries {
			return nil, fmt.Errorf("archive exceeds %d entry limit", limits.entries)
		}

		isDir := header.Typeflag == tar.TypeDir
		isRegular := header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA
		if !isDir && !isRegular {
			return nil, fmt.Errorf("unsupported tar entry type for %q", header.Name)
		}
		if header.Size < 0 {
			return nil, fmt.Errorf("invalid tar entry size for %q", header.Name)
		}
		if isDir && header.Size != 0 {
			return nil, fmt.Errorf("tar directory %q contains data", header.Name)
		}

		name, err := safepath.CanonicalRelativePath(header.Name, isDir)
		if err != nil {
			return nil, fmt.Errorf("invalid tar entry path %q: %w", header.Name, err)
		}
		if name == "." {
			continue
		}
		if isReservedAssetsPath(name) {
			return nil, fmt.Errorf("archive entry %q uses the reserved %q path", name, reservedAssetsPath)
		}
		if err := paths.add(name, isDir); err != nil {
			return nil, err
		}
		if isDir {
			continue
		}

		readLimit, err := computeReadLimit(totalSize, limits)
		if err != nil {
			return nil, err
		}
		// Refuse before allocating anything when the tar header's own declared
		// size already cannot fit. This is the archive-bomb OOM fix: an entry
		// whose header already declares more than the per-file/aggregate cap
		// used to be read in full (io.ReadAll's growth strategy) before that
		// size could be checked, so refusal happened only after the
		// allocation, not instead of it.
		declaredSize, err := checkDeclaredSize(name, uint64(header.Size), readLimit)
		if err != nil {
			return nil, err
		}
		content, err := readFileContent(tarReader, name, declaredSize)
		if tarStreamExceeded(physicalStream) {
			return nil, tarStreamLimitError(limits.tarStream)
		}
		if err != nil {
			return nil, err
		}
		totalSize += int64(len(content))
		if shouldSkip(name) {
			continue
		}
		files[name] = content
	}
}

func extractZip(archiveBytes []byte, limits extractLimits) (map[string][]byte, error) {
	zipReader, err := zip.NewReader(bytes.NewReader(archiveBytes), int64(len(archiveBytes)))
	if err != nil {
		return nil, fmt.Errorf("open zip archive: %w", err)
	}

	files := make(map[string][]byte)
	paths := newArchivePathTracker(limits.pathMetadata)
	var totalSize int64

	for index, file := range zipReader.File {
		if index+1 > limits.entries {
			return nil, fmt.Errorf("archive exceeds %d entry limit", limits.entries)
		}

		mode := file.Mode()
		isDir := file.FileInfo().IsDir()
		if !isDir && !mode.IsRegular() {
			return nil, fmt.Errorf("unsupported zip entry type for %q", file.Name)
		}
		if isDir && file.UncompressedSize64 != 0 {
			return nil, fmt.Errorf("zip directory %q contains data", file.Name)
		}

		name, err := safepath.CanonicalRelativePath(file.Name, isDir)
		if err != nil {
			return nil, fmt.Errorf("invalid zip entry path %q: %w", file.Name, err)
		}
		if name == "." {
			continue
		}
		if isReservedAssetsPath(name) {
			return nil, fmt.Errorf("archive entry %q uses the reserved %q path", name, reservedAssetsPath)
		}
		if err := paths.add(name, isDir); err != nil {
			return nil, err
		}
		if isDir {
			continue
		}

		readLimit, err := computeReadLimit(totalSize, limits)
		if err != nil {
			return nil, err
		}
		// Refuse by the declared UncompressedSize64 before opening the entry
		// at all, so a hostile entry never even starts decompressing. The
		// declaration can still lie (a small declared size hiding a much
		// larger real decompressed stream), so readFileContent below still
		// bounds the actual read independently.
		declaredSize, err := checkDeclaredSize(name, file.UncompressedSize64, readLimit)
		if err != nil {
			return nil, err
		}

		reader, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("open zip entry %q: %w", file.Name, err)
		}
		content, readErr := readFileContent(reader, name, declaredSize)
		closeErr := reader.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close zip entry %q: %w", file.Name, closeErr)
		}

		totalSize += int64(len(content))
		if shouldSkip(name) {
			continue
		}
		files[name] = content
	}

	return files, nil
}

type archivePathKind uint8

const (
	archiveImplicitDir archivePathKind = iota + 1
	archiveExplicitDir
	archiveRegularFile
)

type archivePathTracker struct {
	entries       map[string]archivePathKind
	metadataUsed  int64
	metadataLimit int64
}

func newArchivePathTracker(metadataLimit int64) *archivePathTracker {
	return &archivePathTracker{
		entries:       make(map[string]archivePathKind),
		metadataLimit: metadataLimit,
	}
}

func (t *archivePathTracker) add(name string, directory bool) error {
	components := strings.Split(name, "/")
	for index := 1; index < len(components); index++ {
		ancestor := strings.Join(components[:index], "/")
		switch t.entries[ancestor] {
		case archiveRegularFile:
			return fmt.Errorf("archive path %q has file ancestor %q", name, ancestor)
		case 0:
			if err := t.insert(ancestor, archiveImplicitDir); err != nil {
				return err
			}
		}
	}

	existing := t.entries[name]
	if directory {
		switch existing {
		case 0:
			return t.insert(name, archiveExplicitDir)
		case archiveImplicitDir:
			t.entries[name] = archiveExplicitDir
			return nil
		case archiveExplicitDir:
			return fmt.Errorf("duplicate archive path %q", name)
		default:
			return fmt.Errorf("archive path %q is both a file and directory", name)
		}
	}

	switch existing {
	case 0:
		return t.insert(name, archiveRegularFile)
	case archiveRegularFile:
		return fmt.Errorf("duplicate archive path %q", name)
	default:
		return fmt.Errorf("archive path %q is both a file and directory", name)
	}
}

func (t *archivePathTracker) insert(name string, kind archivePathKind) error {
	cost := int64(len(name)) + pathMetadataEntryOverhead
	if t.metadataLimit < 0 || t.metadataUsed > t.metadataLimit || cost > t.metadataLimit-t.metadataUsed {
		return fmt.Errorf("archive path metadata exceeds %d byte limit", t.metadataLimit)
	}
	// Charge the complete cost before growing the map. Existing keys never
	// reach this helper, so each explicit path or implicit ancestor is counted
	// exactly once.
	t.metadataUsed += cost
	t.entries[name] = kind
	return nil
}

func tarStreamExceeded(stream *io.LimitedReader) bool {
	return stream.N == 0
}

func tarStreamLimitError(limit int64) error {
	return fmt.Errorf("decompressed tar stream exceeds %d byte limit", limit)
}

// computeReadLimit derives the ceiling a single entry's content may occupy:
// whichever is smaller of the per-file cap and whatever remains of the
// aggregate uncompressed budget before this entry.
func computeReadLimit(totalSize int64, limits extractLimits) (int64, error) {
	remainingTotal := limits.total - totalSize
	if remainingTotal < 0 {
		return 0, fmt.Errorf("archive exceeds %d byte uncompressed size limit", limits.total)
	}
	readLimit := limits.file
	if remainingTotal < readLimit {
		readLimit = remainingTotal
	}
	return readLimit, nil
}

// checkDeclaredSize refuses an entry before any of its content is read, when
// its own declared size (a tar header's Size, or a zip entry's
// UncompressedSize64) already cannot fit the remaining budget. declared is
// untrusted input and is compared in uint64 space so an implausibly large
// declared value cannot overflow into a false pass. This is what makes
// refusal happen instead of an allocation, not after one: previously an
// oversized entry's full content was read into memory before its size could
// be checked, which is what let a single archive entry over the per-file cap
// (or an entry with a lying header) OOM the process instead of being
// refused cheaply.
func checkDeclaredSize(name string, declared uint64, readLimit int64) (int64, error) {
	if readLimit < 0 || declared > uint64(readLimit) {
		return 0, fmt.Errorf("archive entry %q declares %d bytes, exceeding the %d byte limit", name, declared, readLimit)
	}
	return int64(declared), nil
}

// readFileContent reads exactly declaredSize bytes for one entry, having
// already been cleared by checkDeclaredSize. It allocates once, sized to the
// entry's own declared size rather than the (potentially much larger)
// readLimit, and never grows that buffer the way io.ReadAll's doubling
// strategy would — so peak memory for this entry is bounded to what it
// actually claims to contain, not the per-file/aggregate ceiling.
//
// A declared size can still lie in the other direction: an entry may claim
// fewer bytes than it actually streams, hiding a decompression bomb under a
// small declared size. After the declared bytes are read, one more byte is
// probed for defensively — cheap regardless of outcome, since a well-behaved
// tar or zip reader reports EOF at the entry boundary without doing further
// work — so a lying declaration is caught at the cost of one byte, not the
// size of the hidden payload.
func readFileContent(r io.Reader, name string, declaredSize int64) ([]byte, error) {
	content := make([]byte, declaredSize)
	n, err := io.ReadFull(r, content)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, fmt.Errorf("read archive entry %q: %w", name, err)
	}
	if int64(n) != declaredSize {
		return nil, fmt.Errorf("archive entry %q declares %d bytes but the stream ended after %d", name, declaredSize, n)
	}

	var probe [1]byte
	extraN, extraErr := r.Read(probe[:])
	if extraN > 0 {
		return nil, fmt.Errorf("archive entry %q declares %d bytes but the stream produced more", name, declaredSize)
	}
	if extraErr != nil && extraErr != io.EOF {
		return nil, fmt.Errorf("read archive entry %q: %w", name, extraErr)
	}

	return content, nil
}

func readAtMost(r io.Reader, limit int64) ([]byte, error) {
	if limit < 0 {
		return nil, fmt.Errorf("invalid byte limit")
	}
	content, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > limit {
		return nil, fmt.Errorf("content exceeds %d byte limit", limit)
	}
	return content, nil
}

func shouldSkip(name string) bool {
	base := path.Base(name)
	return base == ".DS_Store" || strings.HasPrefix(base, "._")
}

// reservedAssetsPath is the top-level name reserved for site-facing assets
// served at /{site}/_assets/{id}: an archive containing it (as a file or a
// directory, or anything under it) is refused rather than extracted, so an
// uploaded site version can never shadow the asset route with content of
// its own.
const reservedAssetsPath = "_assets"

func isReservedAssetsPath(name string) bool {
	return name == reservedAssetsPath || strings.HasPrefix(name, reservedAssetsPath+"/")
}
