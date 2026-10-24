package handler

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"mime/multipart"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func photoJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := jpeg.Encode(&out, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func photoWebP(t *testing.T, kind string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/photos/" + kind + ".webp")
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func photoChunk(b []byte, kind string, data []byte) []byte {
	out := append([]byte{}, b...)
	out = append(out, []byte(kind)...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(data)))
	out = append(out, data...)
	if len(data)%2 != 0 {
		out = append(out, 0)
	}
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(out)-8))
	return out
}
func photoAPP1(b []byte, payload string) []byte {
	out := append([]byte{}, b[:2]...)
	out = append(out, 255, 225)
	out = binary.BigEndian.AppendUint16(out, uint16(len(payload)+2))
	out = append(out, payload...)
	return append(out, b[2:]...)
}
func photoMultipart(t *testing.T, names []string, data [][]byte) (string, string) {
	t.Helper()
	var out bytes.Buffer
	writer := multipart.NewWriter(&out)
	for i, name := range names {
		part, err := writer.CreateFormFile("photos", name)
		if err != nil {
			t.Fatal(err)
		}
		part.Write(data[i])
	}
	writer.Close()
	return out.String(), writer.FormDataContentType()
}
func TestPhotoValidation(t *testing.T) {
	jpg := photoJPEG(t, 8, 6)
	webp := photoWebP(t, "lossy")
	for _, kind := range []string{"lossy", "lossless", "alpha"} {
		cfg, ext, err := validatePhoto(photoWebP(t, kind), 2048)
		if err != nil || ext != "webp" || cfg.Width != 8 || cfg.Height != 6 {
			t.Fatalf("%s: %+v %s %v", kind, cfg, ext, err)
		}
	}
	cases := map[string][]byte{
		"text": []byte("not an image"), "svg": []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), "html": []byte("<html>hello</html>"),
		"png": {137, 80, 78, 71, 13, 10, 26, 10}, "gif": []byte("GIF89a"),
		"jpeg polyglot":    append(append([]byte{}, jpg...), []byte("<html>hello</html>")...),
		"jpeg header html": append([]byte{255, 216, 255}, []byte("<html>hello</html>")...),
		"jpeg truncated":   jpg[:len(jpg)-3], "webp truncated": webp[:len(webp)-3],
		"jpeg exif": photoAPP1(jpg, "Exif\x00\x00GPS"), "jpeg xmp": photoAPP1(jpg, "http://ns.adobe.com/xap/1.0/\x00GPS"),
		"webp exif": photoChunk(webp, "EXIF", []byte("GPS")), "webp xmp": photoChunk(webp, "XMP ", []byte("GPS")),
		"webp animation": photoChunk(webp, "ANIM", make([]byte, 6)),
		"webp payload":   photoChunk(webp, "HTML", []byte("<html/>")),
		"webp trailing":  append(append([]byte{}, webp...), []byte("<html/>")...),
		"dimensions":     photoJPEG(t, 2049, 1),
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := validatePhoto(b, 2048); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestPhotosWebPExtendedHeader(t *testing.T) {
	original := photoWebP(t, "alpha")
	for _, flag := range []byte{2, 4, 8} {
		b := append([]byte{}, original...)
		b[20] |= flag
		if _, _, err := validatePhoto(b, 2048); err == nil {
			t.Fatalf("accepted VP8X flag %d", flag)
		}
	}
	b := append([]byte{}, original...)
	b[24] = 0 // Contradicts the actual image width.
	if _, _, err := validatePhoto(b, 2048); err == nil {
		t.Fatal("accepted contradictory WebP dimensions")
	}
}

func TestPhotosRejectDuplicateJPEGFrame(t *testing.T) {
	jpg := photoJPEG(t, 8, 6)
	off := bytes.Index(jpg, []byte{255, 192})
	if off < 0 {
		t.Fatal("fixture has no frame header")
	}
	n := 2 + int(binary.BigEndian.Uint16(jpg[off+2:off+4]))
	duplicate := append([]byte{}, jpg[:off]...)
	duplicate = append(duplicate, jpg[off:off+n]...)
	duplicate = append(duplicate, jpg[off:]...)
	// JFIF makes DecodeConfig stop at the first frame, before the duplicate.
	jfif := []byte{255, 224, 0, 16, 'J', 'F', 'I', 'F', 0, 1, 1, 0, 0, 1, 0, 1, 0, 0}
	data := append([]byte{}, duplicate[:2]...)
	data = append(data, jfif...)
	data = append(data, duplicate[2:]...)
	if _, err := jpeg.DecodeConfig(bytes.NewReader(data)); err != nil {
		t.Fatalf("fixture must pass header-only decode: %v", err)
	}
	if _, _, err := validatePhoto(data, 2048); err == nil {
		t.Fatal("accepted duplicate JPEG frame")
	}
}
func TestPhotoCollisionIndex(t *testing.T) {
	files := map[string][]byte{"images/front.jpg": nil, "images/dir.jpg/child": nil}
	for i := 2; i <= 20000; i++ {
		files[fmt.Sprintf("images/front-%d.jpg", i)] = nil
	}
	occupied, err := photoOccupiedPaths(context.Background(), files)
	if err != nil {
		t.Fatal(err)
	}
	path, err := nextPhotoPath(context.Background(), occupied, "front.jpg")
	if err != nil || path != "images/front-20001.jpg" || !occupied["images/dir.jpg"] {
		t.Fatalf("collision: %s %v", path, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := photoOccupiedPaths(ctx, files); !errors.Is(err, context.Canceled) {
		t.Fatal("index ignored cancellation")
	}
	if _, err := nextPhotoPath(ctx, occupied, "front.jpg"); !errors.Is(err, context.Canceled) {
		t.Fatal("search ignored cancellation")
	}
}

func TestPhotoMetadataAllowlist(t *testing.T) {
	jpg := photoJPEG(t, 8, 6)
	icc := append([]byte("ICC_PROFILE\x00\x01\x01"), make([]byte, 16384-14)...)
	// The container and size are checked, not the profile contents.
	copy(icc[14:], "arbitrary ICC contents")
	jfif := append([]byte("JFIF\x00"), make([]byte, 9)...)
	adobe := append([]byte("Adobe"), make([]byte, 7)...)
	for _, b := range [][]byte{photoAPP(jpg, 0xe0, jfif), photoAPP(jpg, 0xe2, icc), photoAPP(jpg, 0xee, adobe)} {
		if _, _, err := validatePhoto(b, 2048); err != nil {
			t.Fatal(err)
		}
	}
	for marker := byte(0xe0); marker <= 0xef; marker++ {
		if marker == 0xe0 || marker == 0xe2 || marker == 0xee {
			continue
		}
		if _, _, err := validatePhoto(photoAPP(jpg, marker, []byte("private")), 2048); err == nil {
			t.Fatalf("APP%x accepted", marker)
		}
	}
	for _, b := range [][]byte{photoAPP(jpg, 0xfe, []byte("comment")), photoAPP(photoAPP(jpg, 0xe2, icc), 0xe2, icc), photoAPP(jpg, 0xee, []byte("private")), append(bytes.Clone(jpg), 'x')} {
		if _, _, err := validatePhoto(b, 2048); err == nil {
			t.Fatal("JPEG metadata accepted")
		}
	}
	base := photoWebP(t, "alpha")
	for _, size := range []int{16384, 16385} {
		profile := make([]byte, size)
		copy(profile, "arbitrary ICC contents")
		b := photoChunk(base[:30], "ICCP", profile)
		b[20] |= 0x20
		b = append(b, base[30:]...)
		binary.LittleEndian.PutUint32(b[4:8], uint32(len(b)-8))
		_, _, err := validatePhoto(b, 2048)
		if (err == nil) != (size == 16384) {
			t.Fatalf("WebP ICC size %d: %v", size, err)
		}
	}
	for _, kind := range []string{"EXIF", "XMP ", "JUNK"} {
		if _, _, err := validatePhoto(photoChunk(base, kind, []byte("private")), 2048); err == nil {
			t.Fatalf("WebP %s accepted", kind)
		}
	}
}

func TestPhotoCanvasFixtures(t *testing.T) {
	paths, err := filepath.Glob("testdata/photos/canvas-*.*")
	if err != nil || len(paths) == 0 {
		t.Fatal("missing canvas fixtures")
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := validatePhoto(b, 2048); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPhotoDecoderSafety(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node unavailable")
	}
	cmd := exec.Command("node", "../../scripts/test-photo-decoders.cjs")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("decoder tests: %v\n%s", err, out)
	}
}

func photoAPP(b []byte, marker byte, payload []byte) []byte {
	out := append([]byte{}, b[:2]...)
	out = append(out, 255, marker)
	out = binary.BigEndian.AppendUint16(out, uint16(len(payload)+2))
	out = append(out, payload...)
	return append(out, b[2:]...)
}

func TestPhotoPathConflicts(t *testing.T) {
	for _, files := range []map[string][]byte{{"images": nil}, {"images/front.jpg/child": nil}} {
		occupied, err := photoOccupiedPaths(context.Background(), files)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := nextPhotoPath(context.Background(), occupied, "front.jpg"); err == nil {
			t.Fatal("file/directory conflict accepted")
		}
	}
}
