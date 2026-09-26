package tarball

import (
	"fmt"
	"path/filepath"
	"strings"
)

// blockedExtensions is a denylist, not an allowlist: Simple Host serves
// uploads as static bytes (no server-side execution), so the worst a
// hostile file can do is run on a visitor's machine if they choose to
// download it. We accept almost everything a site is made of (HTML, CSS,
// JS, images, fonts, audio, video, PDFs, wasm) and ordinary downloads (ZIP,
// DMG), and reject two groups:
//   - source-script types, a guardrail against uploading a source tree or a
//     server entrypoint;
//   - Windows and other installable or directly executable files, the usual
//     malware carriers, which a trusted company hostname would otherwise
//     lend credibility to. Disk images (.iso, .img) are included because
//     they are a common way to deliver those past mail filters.
//
// This is a guardrail, not a malware defence: CLAMD_ADDR scans the bytes.
// .js stays allowed: it is web content.
var blockedExtensions = map[string]struct{}{
	// Source scripts.
	".sh": {}, ".bash": {}, ".zsh": {}, ".fish": {}, ".bat": {}, ".cmd": {},
	".ps1": {}, ".py": {}, ".pyc": {}, ".rb": {}, ".pl": {}, ".go": {}, ".php": {},
	// Windows executables, installers and script hosts.
	".exe": {}, ".dll": {}, ".msi": {}, ".msix": {}, ".appx": {}, ".scr": {},
	".com": {}, ".pif": {}, ".cpl": {}, ".hta": {}, ".vbs": {}, ".vbe": {},
	".jse": {}, ".wsf": {}, ".wsh": {}, ".lnk": {}, ".reg": {},
	// Other platforms' packages and disk images.
	".jar": {}, ".apk": {}, ".aab": {}, ".pkg": {}, ".deb": {}, ".rpm": {},
	".iso": {}, ".img": {},
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
