package handler

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"golang.org/x/image/webp"
	"image"
	"image/jpeg"
	"io"
	"strings"
)

// The value distinguishes directories from files. Build prefixes once, then
// each suffix probe is one map lookup, even in a large, collision-heavy tree.
func photoOccupiedPaths(ctx context.Context, files map[string][]byte) (map[string]bool, error) {
	occupied := make(map[string]bool, len(files))
	for path := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		occupied[path] = false
		for i := strings.IndexByte(path, '/'); i >= 0; {
			occupied[path[:i]] = true
			next := strings.IndexByte(path[i+1:], '/')
			if next < 0 {
				break
			}
			i += next + 1
		}
	}
	return occupied, nil
}

func nextPhotoPath(ctx context.Context, occupied map[string]bool, name string) (string, error) {
	if dir, exists := occupied["images"]; exists && !dir {
		return "", errors.New("The images path is a file. Rename it before adding photos.")
	}
	ext := name[strings.LastIndexByte(name, '.'):]
	stem := strings.TrimSuffix(name, ext)
	path := "images/" + name
	for n := 2; ; n++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		dir, taken := occupied[path]
		if !taken {
			return path, nil
		}
		if dir {
			return "", errors.New("A folder uses this photo name. Choose another name.")
		}
		suffix := fmt.Sprintf("-%d", n)
		base := stem
		if len(base)+len(suffix) > 60 {
			base = base[:60-len(suffix)]
		}
		path = "images/" + base + suffix + ext
	}
}

var errPhotoContainer = errors.New("This photo is incomplete or is not a supported WebP or JPEG image.")
var errPhotoMetadata = errors.New("This photo still contains camera or location metadata. Choose it again to make a clean copy.")

func validatePhoto(data []byte, maxDimension int) (image.Config, string, error) {
	var cfg image.Config
	var err error
	ext := ""
	switch {
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		ext = "webp"
		if err = checkPhotoWebP(data); err == nil {
			cfg, err = webp.DecodeConfig(bytes.NewReader(data))
		}
	case len(data) >= 3 && bytes.Equal(data[:3], []byte{255, 216, 255}):
		ext = "jpg"
		if err = checkPhotoJPEG(data); err == nil {
			cfg, err = jpeg.DecodeConfig(bytes.NewReader(data))
		}
	default:
		err = errPhotoContainer
	}
	if err != nil {
		if errors.Is(err, errPhotoMetadata) {
			return cfg, ext, err
		}
		return cfg, ext, errPhotoContainer
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > maxDimension || cfg.Height > maxDimension {
		return cfg, ext, fmt.Errorf("Photos must be no more than %d pixels on their longest side.", maxDimension)
	}
	return cfg, ext, nil
}

// Walk all RIFF chunks, not just the first header read by DecodeConfig.
func checkPhotoWebP(b []byte) error {
	if uint64(binary.LittleEndian.Uint32(b[4:8]))+8 != uint64(len(b)) {
		return errPhotoContainer
	}
	imageSeen, extended, alpha, icc := false, false, false, false
	var canvasWidth, canvasHeight int
	var flags byte
	for off := 12; off < len(b); {
		if len(b)-off < 8 {
			return errPhotoContainer
		}
		kind := string(b[off : off+4])
		n := uint64(binary.LittleEndian.Uint32(b[off+4 : off+8]))
		off += 8
		if n > uint64(len(b)-off) {
			return errPhotoContainer
		}
		chunk := b[off : off+int(n)]
		switch kind {
		case "EXIF", "XMP ":
			return errPhotoMetadata
		case "VP8X":
			if off != 20 || extended || n != 10 {
				return errPhotoContainer
			}
			extended = true
			flags = chunk[0]
			canvasWidth = 1 + int(chunk[4]) + int(chunk[5])<<8 + int(chunk[6])<<16
			canvasHeight = 1 + int(chunk[7]) + int(chunk[8])<<8 + int(chunk[9])<<16
			if chunk[0]&0x0c != 0 {
				return errPhotoMetadata
			}
			if chunk[0]&0xc3 != 0 || chunk[1] != 0 || chunk[2] != 0 || chunk[3] != 0 {
				return errPhotoContainer
			}
		case "VP8 ", "VP8L":
			if imageSeen || n == 0 {
				return errPhotoContainer
			}
			imageSeen = true
			// A VP8 keyframe's first partition must fit inside the chunk.
			if kind == "VP8 " && (n < 10 || chunk[0]&1 != 0 || int(chunk[0]>>5)|int(chunk[1])<<3|int(chunk[2])<<11 > len(chunk)-10) {
				return errPhotoContainer
			}
			if kind == "VP8L" && (n < 5 || chunk[0] != 0x2f || alpha) {
				return errPhotoContainer
			}
			// DecodeConfig stops at VP8X in x/image. Also parse the actual
			// image header so a small canvas cannot hide a larger payload.
			header := make([]byte, 20)
			copy(header, "RIFF")
			binary.LittleEndian.PutUint32(header[4:8], uint32(n)+12)
			copy(header[8:], "WEBP"+kind)
			binary.LittleEndian.PutUint32(header[16:20], uint32(n))
			cfg, err := webp.DecodeConfig(io.MultiReader(bytes.NewReader(header), bytes.NewReader(chunk)))
			if err != nil || extended && (cfg.Width != canvasWidth || cfg.Height != canvasHeight) {
				return errPhotoContainer
			}
			if extended && ((flags&0x10 != 0) != (alpha || kind == "VP8L" && chunk[4]&0x10 != 0)) {
				return errPhotoContainer
			}
		case "ALPH":
			if !extended || imageSeen || alpha || n == 0 || chunk[0]&0xc3 > 1 {
				return errPhotoContainer
			}
			alpha = true
			if chunk[0]&3 == 0 && n != uint64(1+canvasWidth*canvasHeight) {
				return errPhotoContainer
			}
		case "ICCP":
			if !extended || imageSeen || alpha || icc || n == 0 || n > 16*1024 {
				return errPhotoContainer
			}
			icc = true
		default:
			return errPhotoContainer // Includes animation chunks and extra payloads.
		}
		off += int(n)
		if n&1 != 0 {
			if off >= len(b) || b[off] != 0 {
				return errPhotoContainer
			}
			off++
		}
	}
	if !imageSeen || extended && (flags&0x20 != 0) != icc {
		return errPhotoContainer
	}
	return nil
}

// Validate the marker stream through EOI, including entropy byte stuffing and
// scan boundaries. DecodeConfig then validates the image header. No pixel decode.
func checkPhotoJPEG(b []byte) error {
	scans, iccBytes := 0, 0
	frameSeen := false
	for off := 2; off < len(b); {
		if b[off] != 255 {
			return errPhotoContainer
		}
		for off < len(b) && b[off] == 255 {
			off++
		}
		if off >= len(b) {
			return errPhotoContainer
		}
		marker := b[off]
		off++
		if marker == 0xd9 {
			if off == len(b) && scans > 0 {
				return nil
			}
			return errPhotoContainer
		}
		if off+2 > len(b) {
			return errPhotoContainer
		}
		n := int(binary.BigEndian.Uint16(b[off : off+2]))
		if n < 2 || n > len(b)-off {
			return errPhotoContainer
		}
		payload := b[off+2 : off+n]
		switch marker {
		case 0xe1:
			return errPhotoMetadata // All APP1, including Exif and both XMP forms.
		case 0xe0:
			if len(payload) < 14 || string(payload[:5]) != "JFIF\x00" || len(payload) != 14+3*int(payload[12])*int(payload[13]) {
				return errPhotoContainer
			}
		case 0xe2:
			if len(payload) < 14 || string(payload[:12]) != "ICC_PROFILE\x00" {
				return errPhotoContainer
			}
			iccBytes += len(payload)
			if iccBytes > 16*1024 {
				return errPhotoMetadata
			}
		case 0xee:
			if len(payload) != 12 || string(payload[:5]) != "Adobe" {
				return errPhotoMetadata
			}
		case 0xc0, 0xc1, 0xc2:
			if frameSeen || scans != 0 {
				return errPhotoContainer
			}
			frameSeen = true
		case 0xda:
			if !frameSeen || len(payload) < 6 || payload[0] < 1 || payload[0] > 4 || len(payload) != 4+2*int(payload[0]) {
				return errPhotoContainer
			}
		case 0xc4, 0xdb, 0xdd:
		default:
			return errPhotoContainer // No comments or arbitrary application data.
		}
		off += n
		if marker == 0xda {
			scans++
			start := off
			for off < len(b) {
				if b[off] != 255 {
					off++
					continue
				}
				if off+1 >= len(b) {
					return errPhotoContainer
				}
				if b[off+1] == 0 || b[off+1] >= 0xd0 && b[off+1] <= 0xd7 {
					off += 2
					continue
				}
				break
			}
			if off == start {
				return errPhotoContainer
			}
		}
	}
	return errPhotoContainer
}
