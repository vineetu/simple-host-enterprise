// Package safepath contains validation shared by HTTP and filesystem
// boundaries. Validation is intentionally lexical; filesystem confinement is
// still enforced independently by the storage layer.
package safepath

import (
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxSegmentBytes           = 255
	MaxRelativePathBytes      = 1_024
	MaxRelativePathDepth      = 32
	MaxRelativeComponentBytes = MaxSegmentBytes
)

// ValidateSegment accepts one filesystem path component, never a path.
func ValidateSegment(value string) error {
	switch {
	case value == "":
		return fmt.Errorf("path segment is empty")
	case value == "." || value == "..":
		return fmt.Errorf("path segment %q is reserved", value)
	case len(value) > MaxSegmentBytes:
		return fmt.Errorf("path segment exceeds %d bytes", MaxSegmentBytes)
	case !utf8.ValidString(value):
		return fmt.Errorf("path segment is not valid UTF-8")
	case strings.ContainsAny(value, `/\\`):
		return fmt.Errorf("path segment contains a separator")
	}

	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("path segment contains a control character")
		}
	}
	return nil
}

func IsSegment(value string) bool {
	return ValidateSegment(value) == nil
}

// CanonicalRelativePath validates an archive/storage-relative slash-separated
// path. Leading "./" markers and a directory's final slash are normalized for
// compatibility with common archive tools. All other non-canonical forms are
// rejected. A root directory entry is returned as ".".
func CanonicalRelativePath(raw string, directory bool) (string, error) {
	if raw == "" || !utf8.ValidString(raw) {
		return "", fmt.Errorf("relative path has invalid encoding")
	}
	if len(raw) > MaxRelativePathBytes {
		return "", fmt.Errorf("relative path exceeds %d bytes", MaxRelativePathBytes)
	}
	if strings.ContainsRune(raw, '\\') || strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("path %q is not relative", raw)
	}
	for _, r := range raw {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("path %q contains a control character", raw)
		}
	}

	value := raw
	for strings.HasPrefix(value, "./") {
		value = strings.TrimPrefix(value, "./")
	}
	if directory {
		value = strings.TrimSuffix(value, "/")
	} else if strings.HasSuffix(value, "/") {
		return "", fmt.Errorf("regular path %q has a directory suffix", raw)
	}
	if value == "" || value == "." {
		if directory {
			return ".", nil
		}
		return "", fmt.Errorf("relative path %q is empty", raw)
	}
	if len(value) > MaxRelativePathBytes || path.Clean(value) != value || value == ".." || strings.HasPrefix(value, "../") {
		return "", fmt.Errorf("path %q is not canonical", raw)
	}

	components := strings.Split(value, "/")
	if len(components) > MaxRelativePathDepth {
		return "", fmt.Errorf("path %q exceeds depth %d", raw, MaxRelativePathDepth)
	}
	for _, component := range components {
		if err := ValidateSegment(component); err != nil {
			return "", fmt.Errorf("path %q has an invalid component: %w", raw, err)
		}
	}
	return value, nil
}
