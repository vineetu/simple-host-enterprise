package tarball

import (
	"fmt"
	"path/filepath"
	"strings"
)

// blockedExtensions is a denylist, not an allowlist: Simple Host serves
// uploads as static bytes (no server-side execution), so the worst a
// hostile file can do is run on a visitor's machine if they choose to
// download it — the same risk as any file share. We therefore accept
// almost everything (HTML/CSS/JS, images, fonts, audio, video, PDFs,
// wasm, and binary downloads like .dmg/.jar/.apk) and only reject a
// small set of source-script types (a guardrail against someone
// accidentally uploading a source tree or a server entrypoint) plus
// Windows .exe.
var blockedExtensions = map[string]struct{}{
	".sh":   {},
	".bash": {},
	".zsh":  {},
	".fish": {},
	".bat":  {},
	".cmd":  {},
	".ps1":  {},
	".py":   {},
	".pyc":  {},
	".rb":   {},
	".pl":   {},
	".go":   {},
	".php":  {},
	".exe":  {},
}

func ValidateExtensions(files map[string][]byte) error {
	for path := range files {
		extension := strings.ToLower(filepath.Ext(path))
		if _, ok := blockedExtensions[extension]; ok {
			return fmt.Errorf("disallowed file extension for %q", path)
		}
	}

	return nil
}
