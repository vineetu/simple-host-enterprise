package tarball

import "testing"

func TestValidateExtensions(t *testing.T) {
	for _, name := range []string{"setup.EXE", "a/b.dll", "x.msi", "run.hta", "s.vbs", "app.jar", "app.apk", "disk.iso", "boot.img", "go.lnk", "deploy.sh"} {
		if err := ValidateExtensions(map[string][]byte{name: nil}); err == nil {
			t.Errorf("%s: accepted, want refused", name)
		}
	}
	for _, name := range []string{"index.html", "app.js", "style.css", "release.zip", "App.dmg", "doc.pdf", "mod.wasm", "photo.png"} {
		if err := ValidateExtensions(map[string][]byte{name: nil}); err != nil {
			t.Errorf("%s: %v, want accepted", name, err)
		}
	}
}
