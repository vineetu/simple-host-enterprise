package storage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vsriram/simple-host/internal/safepath"
)

// A version is stored as one gzipped tar object. One object per version
// makes a deploy a single atomic PUT (a reader sees the whole version or no
// object at all), a cache fill a single GET, and a restore a single
// server-side copy. The archive is unpacked only by scanVersionArchive, which
// canonicalises every entry path, refuses anything but regular files and
// directories, refuses duplicates and file/directory conflicts, bounds the
// totals, and writes through an *os.Root so nothing can land outside the
// destination — the same guarantees the site tree on disk used to give.

// Bounds applied when a version archive is unpacked. Deploys are already
// held to smaller limits by internal/tarball; these exist so a tampered or
// corrupt object cannot fill the cache volume.
const (
	maxVersionFiles       = 100_000
	maxVersionBytes       = 1 << 30
	maxVersionObjectBytes = 1 << 30
)

// buildVersionArchive packs a deploy's files into the stored archive form.
// Every name must already be canonical (internal/tarball produces them so);
// anything else is refused rather than normalised.
func buildVersionArchive(files map[string][]byte) ([]byte, regularFileStats, error) {
	names := make([]string, 0, len(files))
	for name := range files {
		canonical, err := safepath.CanonicalRelativePath(name, false)
		if err != nil || canonical != name {
			return nil, regularFileStats{}, fmt.Errorf("invalid file path %q", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	tracker := make(storedArchivePathTracker)
	var totals regularFileStats
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, name := range names {
		if err := tracker.add(name, false); err != nil {
			return nil, regularFileStats{}, err
		}
		content := files[name]
		if err := tarWriter.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			return nil, regularFileStats{}, fmt.Errorf("write tar header for %q: %w", name, err)
		}
		if _, err := tarWriter.Write(content); err != nil {
			return nil, regularFileStats{}, fmt.Errorf("write tar entry %q: %w", name, err)
		}
		if err := totals.add(int64(len(content))); err != nil {
			return nil, regularFileStats{}, err
		}
	}
	if err := tarWriter.Close(); err != nil {
		return nil, regularFileStats{}, err
	}
	if err := gzipWriter.Close(); err != nil {
		return nil, regularFileStats{}, err
	}
	return buffer.Bytes(), totals, nil
}

func requireRealDir(parent *os.Root, name string) error {
	info, err := parent.Lstat(name)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%q is not a real directory", name)
	}
	return nil
}

func requireRealFile(parent *os.Root, name string) error {
	info, err := parent.Lstat(name)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%q is not a real regular file", name)
	}
	return nil
}

type regularFileStats struct {
	count int64
	bytes int64
}

func (s *regularFileStats) add(size int64) error {
	if size < 0 || size > int64(^uint64(0)>>1)-s.bytes {
		return fmt.Errorf("regular file byte total overflows")
	}
	s.count++
	s.bytes += size
	return nil
}

func directoryRegularStats(root *os.Root) (regularFileStats, error) {
	var totals regularFileStats
	err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("version entry %q is not a real regular file or directory", name)
		}
		return totals.add(info.Size())
	})
	return totals, err
}

func writeVersionArchive(destination io.Writer, root *os.Root) (regularFileStats, error) {
	var totals regularFileStats
	gzipWriter := gzip.NewWriter(destination)
	tarWriter := tar.NewWriter(gzipWriter)
	walkErr := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("version entry %q is not a real regular file or directory", name)
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("build tar header for %q: %w", name, err)
		}
		header.Name = filepath.ToSlash(name)
		if err := tarWriter.WriteHeader(header); err != nil {
			return fmt.Errorf("write tar header for %q: %w", name, err)
		}
		if info.IsDir() {
			return nil
		}
		file, err := root.Open(name)
		if err != nil {
			return fmt.Errorf("open version file %q: %w", name, err)
		}
		written, copyErr := io.Copy(tarWriter, file)
		closeErr := file.Close()
		if copyErr != nil {
			return fmt.Errorf("archive version file %q: %w", name, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close version file %q: %w", name, closeErr)
		}
		if written != info.Size() {
			return fmt.Errorf("version file %q changed size while archiving", name)
		}
		return totals.add(written)
	})
	if err := tarWriter.Close(); walkErr == nil && err != nil {
		walkErr = err
	}
	if err := gzipWriter.Close(); walkErr == nil && err != nil {
		walkErr = err
	}
	return totals, walkErr
}

type storedArchivePathKind uint8

const (
	storedArchiveImplicitDir storedArchivePathKind = iota + 1
	storedArchiveExplicitDir
	storedArchiveRegularFile
)

type storedArchivePathTracker map[string]storedArchivePathKind

func (t storedArchivePathTracker) add(name string, directory bool) error {
	components := strings.Split(name, "/")
	for index := 1; index < len(components); index++ {
		ancestor := strings.Join(components[:index], "/")
		switch t[ancestor] {
		case storedArchiveRegularFile:
			return fmt.Errorf("archive path %q has file ancestor %q", name, ancestor)
		case 0:
			t[ancestor] = storedArchiveImplicitDir
		}
	}
	existing := t[name]
	if directory {
		switch existing {
		case 0:
			t[name] = storedArchiveExplicitDir
			return nil
		case storedArchiveImplicitDir:
			t[name] = storedArchiveExplicitDir
			return nil
		case storedArchiveExplicitDir:
			return fmt.Errorf("duplicate archive path %q", name)
		default:
			return fmt.Errorf("archive path %q is both a file and directory", name)
		}
	}
	if existing == 0 {
		t[name] = storedArchiveRegularFile
		return nil
	}
	if existing == storedArchiveRegularFile {
		return fmt.Errorf("duplicate archive path %q", name)
	}
	return fmt.Errorf("archive path %q is both a file and directory", name)
}

func scanVersionArchive(source io.Reader, destination *os.Root) (regularFileStats, error) {
	gzipReader, err := gzip.NewReader(source)
	if err != nil {
		return regularFileStats{}, fmt.Errorf("open gzip stream: %w", err)
	}
	tarReader := tar.NewReader(gzipReader)
	paths := make(storedArchivePathTracker)
	var totals regularFileStats
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			if _, err := io.Copy(io.Discard, gzipReader); err != nil {
				gzipReader.Close()
				return regularFileStats{}, fmt.Errorf("finish gzip stream: %w", err)
			}
			if err := gzipReader.Close(); err != nil {
				return regularFileStats{}, fmt.Errorf("close gzip stream: %w", err)
			}
			return totals, nil
		}
		if err != nil {
			gzipReader.Close()
			return regularFileStats{}, fmt.Errorf("read tar entry: %w", err)
		}
		isDirectory := header.Typeflag == tar.TypeDir
		isRegular := header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA
		if !isDirectory && !isRegular {
			gzipReader.Close()
			return regularFileStats{}, fmt.Errorf("unsupported tar entry type for %q", header.Name)
		}
		if header.Size < 0 || (isDirectory && header.Size != 0) {
			gzipReader.Close()
			return regularFileStats{}, fmt.Errorf("invalid tar entry size for %q", header.Name)
		}
		name, err := safepath.CanonicalRelativePath(header.Name, isDirectory)
		if err != nil {
			gzipReader.Close()
			return regularFileStats{}, fmt.Errorf("invalid tar entry path %q: %w", header.Name, err)
		}
		if name == "." {
			continue
		}
		if err := paths.add(name, isDirectory); err != nil {
			gzipReader.Close()
			return regularFileStats{}, err
		}
		if isDirectory {
			if destination != nil {
				// Modes from the archive are not trusted: the cache only
				// ever needs the server to read what it unpacked.
				if err := ensureRootDirectory(destination, name, 0o755); err != nil {
					gzipReader.Close()
					return regularFileStats{}, err
				}
			}
			continue
		}
		if err := totals.add(header.Size); err != nil {
			gzipReader.Close()
			return regularFileStats{}, err
		}
		if totals.count > maxVersionFiles || totals.bytes > maxVersionBytes {
			gzipReader.Close()
			return regularFileStats{}, fmt.Errorf("version archive exceeds %d files or %d bytes", maxVersionFiles, maxVersionBytes)
		}
		if destination == nil {
			if _, err := io.CopyN(io.Discard, tarReader, header.Size); err != nil {
				gzipReader.Close()
				return regularFileStats{}, fmt.Errorf("read tar entry %q: %w", name, err)
			}
			continue
		}
		if err := writeArchiveFile(destination, name, 0o644, tarReader, header.Size); err != nil {
			gzipReader.Close()
			return regularFileStats{}, err
		}
	}
}

func ensureRootDirectory(root *os.Root, name string, finalMode os.FileMode) error {
	parts := strings.Split(name, "/")
	for index := range parts {
		current := strings.Join(parts[:index+1], "/")
		mode := os.FileMode(0o755)
		if index == len(parts)-1 {
			mode = finalMode
		}
		if err := root.Mkdir(current, mode); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create archive directory %q: %w", current, err)
		}
		if err := requireRealDir(root, current); err != nil {
			return fmt.Errorf("inspect archive directory %q: %w", current, err)
		}
	}
	return nil
}

func writeArchiveFile(root *os.Root, name string, mode os.FileMode, source io.Reader, size int64) error {
	parent := path.Dir(name)
	if parent != "." {
		if err := ensureRootDirectory(root, parent, 0o755); err != nil {
			return err
		}
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create archive file %q: %w", name, err)
	}
	_, copyErr := io.CopyN(file, source, size)
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("write archive file %q: %w", name, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close archive file %q: %w", name, closeErr)
	}
	return nil
}

// publishVerifiedArchive owns archiveFile. Raw is not touched until a second

func uniqueName(prefix string) (string, error) {
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate temporary name: %w", err)
	}
	return prefix + hex.EncodeToString(random), nil
}

func removeTree(parent *os.Root, name string) error {
	info, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return parent.Remove(name)
	}

	child, err := parent.OpenRoot(name)
	if err != nil {
		return err
	}
	directory, err := child.Open(".")
	if err != nil {
		child.Close()
		return err
	}
	entries, readErr := directory.ReadDir(-1)
	closeDirErr := directory.Close()
	if readErr != nil {
		child.Close()
		return readErr
	}
	if closeDirErr != nil {
		child.Close()
		return closeDirErr
	}
	for _, entry := range entries {
		if err := removeTree(child, entry.Name()); err != nil {
			child.Close()
			return err
		}
	}
	if err := child.Close(); err != nil {
		return err
	}
	return parent.Remove(name)
}
