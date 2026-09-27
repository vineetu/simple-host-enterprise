package main

import (
	"bytes"
	"os"
	"testing"
)

// docs/advanced/settings.json is exactly what `simple-host settings --json`
// prints. Refresh it with
// go run ./cmd/server settings --json > docs/advanced/settings.json && python3 scripts/settings_docs.py
func TestSettingsJSONMatchesDocs(t *testing.T) {
	want, err := settingsJSON()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("../../docs/advanced/settings.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("docs/advanced/settings.json differs from the settings in code: go run ./cmd/server settings --json > docs/advanced/settings.json && python3 scripts/settings_docs.py")
	}
}
