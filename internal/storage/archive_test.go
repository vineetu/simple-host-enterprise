package storage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// crafted is one hand-built tar entry. Size, when non-zero, overrides the
// header size and only the header block is emitted (no body), which lets a
// test claim a size far larger than it actually ships.
type crafted struct {
	name     string
	typeflag byte
	body     string
	linkname string
	size     int64
}

func craftArchive(t *testing.T, entries ...crafted) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	headerOnly := false
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Typeflag: entry.typeflag, Mode: 0o644, Linkname: entry.linkname, Format: tar.FormatPAX}
		if entry.typeflag == tar.TypeReg {
			header.Size = int64(len(entry.body))
		}
		if entry.size != 0 {
			header.Size = entry.size
			headerOnly = true
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("write header %q: %v", entry.name, err)
		}
		if headerOnly {
			break
		}
		if entry.body != "" {
			if _, err := tw.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !headerOnly {
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
	}
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	gz.Write(raw.Bytes())
	gz.Close()
	return compressed.Bytes()
}

func reg(name, body string) crafted { return crafted{name: name, typeflag: tar.TypeReg, body: body} }
func dir(name string) crafted       { return crafted{name: name, typeflag: tar.TypeDir} }

func TestOpenVersionRefusesUnsafeArchives(t *testing.T) {
	cases := []struct {
		name    string
		archive func(t *testing.T) []byte
	}{
		{"parent traversal", func(t *testing.T) []byte { return craftArchive(t, reg("../escape", "x")) }},
		{"deep parent traversal", func(t *testing.T) []byte { return craftArchive(t, reg("../../../../tmp/escape", "x")) }},
		{"absolute path", func(t *testing.T) []byte { return craftArchive(t, reg("/abs", "x")) }},
		{"traversal via middle", func(t *testing.T) []byte { return craftArchive(t, reg("a/../../b", "x")) }},
		{"non-clean path", func(t *testing.T) []byte { return craftArchive(t, reg("a//b", "x")) }},
		{"backslash", func(t *testing.T) []byte { return craftArchive(t, reg(`..\escape`, "x")) }},
		{"control character", func(t *testing.T) []byte { return craftArchive(t, reg("a\nb", "x")) }},
		{"parent traversal directory", func(t *testing.T) []byte { return craftArchive(t, dir("../d/")) }},
		{"symlink", func(t *testing.T) []byte {
			return craftArchive(t, crafted{name: "link", typeflag: tar.TypeSymlink, linkname: "../../outside"}, reg("link/x", "y"))
		}},
		{"hardlink", func(t *testing.T) []byte {
			return craftArchive(t, crafted{name: "hard", typeflag: tar.TypeLink, linkname: "/etc/passwd"})
		}},
		{"char device", func(t *testing.T) []byte { return craftArchive(t, crafted{name: "dev", typeflag: tar.TypeChar}) }},
		{"block device", func(t *testing.T) []byte { return craftArchive(t, crafted{name: "blk", typeflag: tar.TypeBlock}) }},
		{"fifo", func(t *testing.T) []byte { return craftArchive(t, crafted{name: "fifo", typeflag: tar.TypeFifo}) }},
		{"duplicate file", func(t *testing.T) []byte { return craftArchive(t, reg("a.txt", "one"), reg("a.txt", "two")) }},
		{"duplicate after normalisation", func(t *testing.T) []byte { return craftArchive(t, reg("a.txt", "one"), reg("./a.txt", "two")) }},
		{"duplicate directory", func(t *testing.T) []byte { return craftArchive(t, dir("d/"), dir("d/")) }},
		{"file then directory", func(t *testing.T) []byte { return craftArchive(t, reg("a", "x"), dir("a/")) }},
		{"directory then file", func(t *testing.T) []byte { return craftArchive(t, dir("a/"), reg("a", "x")) }},
		{"implicit directory then file", func(t *testing.T) []byte { return craftArchive(t, reg("a/b", "x"), reg("a", "x")) }},
		{"file under a file", func(t *testing.T) []byte { return craftArchive(t, reg("a", "x"), reg("a/b", "y")) }},
		{"directory with size", func(t *testing.T) []byte {
			return craftArchive(t, crafted{name: "d/", typeflag: tar.TypeDir, size: 10})
		}},
		{"over byte limit", func(t *testing.T) []byte {
			return craftArchive(t, crafted{name: "huge", typeflag: tar.TypeReg, size: maxVersionBytes + 1})
		}},
		{"truncated body", func(t *testing.T) []byte {
			return craftArchive(t, crafted{name: "short", typeflag: tar.TypeReg, size: 4096})
		}},
		{"not gzip", func(t *testing.T) []byte { return []byte("this is not an archive") }},
		{"gzip of garbage", func(t *testing.T) []byte {
			var b bytes.Buffer
			gz := gzip.NewWriter(&b)
			gz.Write(bytes.Repeat([]byte{0xff}, 1024))
			gz.Close()
			return b.Bytes()
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objects := NewMemoryObjects()
			store, cacheDir := newTestStore(t, objects, nil, 1<<30)
			ctx := context.Background()
			key, _ := VersionKey(testSiteA, 1)
			if err := objects.Put(ctx, key, tc.archive(t), ""); err != nil {
				t.Fatal(err)
			}

			if lease, err := store.OpenVersion(ctx, testSiteA, 1); err == nil {
				lease.Close()
				t.Fatal("OpenVersion accepted an unsafe archive")
			}

			// The migration upload path refuses the same bytes and stores nothing.
			if _, err := UploadVersionArchive(ctx, objects, testSiteB, 1, tc.archive(t)); err == nil {
				t.Fatal("UploadVersionArchive accepted an unsafe archive")
			}
			if keys := objects.Keys(); len(keys) != 1 {
				t.Fatalf("refused upload stored objects: %v", keys)
			}

			// Nothing outside the cache dir, and no entry or half-written
			// fill left inside it.
			assertOnlyChild(t, filepath.Dir(cacheDir), "cache")
			assertEmptyDir(t, cacheDir)
			if _, err := os.Lstat("/tmp/escape"); err == nil {
				t.Fatal("/tmp/escape exists")
			}

			// The failed fill is not cached: a valid object at another
			// version, and a repaired object at the same one, both open.
			mustPutVersion(t, store, testSiteA, 2, map[string][]byte{"index.html": []byte("fine")})
			lease := mustOpenVersion(t, store, testSiteA, 2)
			if got := readLeaseFile(t, lease, "index.html"); string(got) != "fine" {
				t.Fatalf("v2 = %q", got)
			}
			lease.Close()
			mustPutVersion(t, store, testSiteA, 1, map[string][]byte{"index.html": []byte("repaired")})
			lease = mustOpenVersion(t, store, testSiteA, 1)
			if got := readLeaseFile(t, lease, "index.html"); string(got) != "repaired" {
				t.Fatalf("v1 = %q", got)
			}
			lease.Close()
		})
	}
}

func TestOpenVersionAcceptsExplicitDirectoriesAndDotSlash(t *testing.T) {
	objects := NewMemoryObjects()
	store, _ := newTestStore(t, objects, nil, 1<<30)
	key, _ := VersionKey(testSiteA, 1)
	archive := craftArchive(t, dir("./"), dir("a/"), reg("a/b.txt", "b"), dir("a/c/"), reg("./top.txt", "top"))
	if err := objects.Put(context.Background(), key, archive, ""); err != nil {
		t.Fatal(err)
	}
	lease := mustOpenVersion(t, store, testSiteA, 1)
	defer lease.Close()
	if got := readLeaseFile(t, lease, "a/b.txt"); string(got) != "b" {
		t.Fatalf("a/b.txt = %q", got)
	}
	if got := readLeaseFile(t, lease, "top.txt"); string(got) != "top" {
		t.Fatalf("top.txt = %q", got)
	}
	info, err := lease.Root().Stat("a/c")
	if err != nil || !info.IsDir() {
		t.Fatalf("a/c: %v, %v", info, err)
	}
}

func TestBuildVersionArchiveRefusesNonCanonicalNames(t *testing.T) {
	for _, name := range []string{"./a", "a//b", "a/", "../a", "/a", "a/./b", "a/../b", "", ".", `a\b`} {
		if _, _, err := buildVersionArchive(map[string][]byte{name: []byte("x")}); err == nil {
			t.Errorf("buildVersionArchive accepted %q", name)
		}
	}
	if _, _, err := buildVersionArchive(map[string][]byte{"a": []byte("x"), "a/b": []byte("y")}); err == nil {
		t.Error("buildVersionArchive accepted a file under a file")
	}
}

func assertOnlyChild(t *testing.T, dir, name string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != name {
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("%s holds %v, want only %s", dir, names, name)
	}
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("%s is not empty: %v", dir, names)
	}
}
