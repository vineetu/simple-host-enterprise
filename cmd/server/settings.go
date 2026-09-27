package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/vsriram/simple-host/internal/config"
	"github.com/vsriram/simple-host/internal/handler"
)

// runSettings is `simple-host settings --json`: every environment setting,
// with its area, description, type, default and range, as JSON
// (docs/advanced/settings.json is this output). Needs no config or database.
func runSettings(args []string) error {
	if len(args) != 1 || args[0] != "--json" {
		return errors.New("usage: simple-host settings --json")
	}
	b, err := settingsJSON()
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(b)
	return err
}

// settingsJSON is config.Settings with each rate limit's default and, for a
// security-sensitive one, its loosest value, from internal/handler.
func settingsJSON() ([]byte, error) {
	defaults := handler.RateLimitDefaults()
	settings := config.Settings()
	for i, s := range settings {
		if s.Type != "rate" {
			continue
		}
		name := ""
		for _, n := range config.RateLimitNames {
			if config.RateLimitEnv(n) == s.Name {
				name = n
			}
		}
		d, ok := defaults[name]
		if !ok {
			return nil, fmt.Errorf("%s has no built-in limit in internal/handler", s.Name)
		}
		settings[i].Default = strconv.Itoa(d.Burst) + "/" + config.ShortDuration(d.Every)
		if loosest, ok := handler.SensitiveRateLimit(name); ok {
			settings[i].Loosest = strconv.Itoa(loosest.Burst) + "/" + config.ShortDuration(loosest.Every)
			settings[i].Security = true
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	err := enc.Encode(config.SettingsDoc{Product: "enterprise", RateFormat: "<burst>/<interval>", Groups: config.SettingGroups, Settings: settings})
	return buf.Bytes(), err
}
