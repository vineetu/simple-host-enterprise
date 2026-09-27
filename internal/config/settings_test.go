package config

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every environment variable this package reads is in the registry, and the
// registry holds nothing it does not read.
func TestSettingsCoverEveryEnvRead(t *testing.T) {
	re := regexp.MustCompile(`(?:os\.Getenv|os\.LookupEnv|getEnvOrDefault|boolEnv|durationEnv|durationIn|int64Env|intIn|need\.require|need\.requireSecret|nonNegativeInt64Env|secretEnv|secretOrEmpty)\("([A-Z][A-Z0-9_]+)"`)
	files, _ := filepath.Glob("*.go")
	read := map[string]bool{}
	for _, n := range RateLimitNames {
		read[RateLimitEnv(n)] = true
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllSubmatch(b, -1) {
			read[string(m[1])] = true
		}
	}
	listed := map[string]bool{}
	for _, s := range Settings() {
		if listed[s.Name] {
			t.Errorf("%s is listed twice", s.Name)
		}
		listed[s.Name] = true
		if s.Description == "" || s.Type == "" {
			t.Errorf("%s needs a description and a type", s.Name)
		}
	}
	var missing, extra []string
	for n := range read {
		if !listed[n] {
			missing = append(missing, n)
		}
	}
	for n := range listed {
		if !read[n] {
			extra = append(extra, n)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("read by config but not in the settings registry (internal/config/settings.go): %v", missing)
	}
	if len(extra) > 0 {
		t.Errorf("in the settings registry but never read: %v", extra)
	}
	for _, n := range RateLimitNames {
		if _, ok := RateLimitDocs[n]; !ok {
			t.Errorf("RateLimitDocs has no entry for %s", n)
		}
	}
}
