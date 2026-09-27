package config

import "testing"

func TestLoadIdleCleanup(t *testing.T) {
	cfg, err := loadIdleCleanup()
	if err != nil || cfg.Days != 0 || cfg.SMTPURL != "" {
		t.Fatalf("default = %+v, %v; want off", cfg, err)
	}
	t.Setenv("IDLE_CLEANUP_DAYS", "180")
	t.Setenv("SMTP_URL", "smtp://mailer:secret@mail.corp.test:587")
	t.Setenv("SMTP_FROM", "Simple Host <hosting@corp.test>")
	if cfg, err = loadIdleCleanup(); err != nil || cfg.Days != 180 || cfg.SMTPFrom == "" {
		t.Fatalf("configured = %+v, %v", cfg, err)
	}
	t.Run("insecure relay", func(t *testing.T) {
		t.Setenv("SMTP_URL", "smtp://relay.corp.test:25?insecure=1")
		if _, err := loadIdleCleanup(); err != nil {
			t.Fatalf("insecure=1 refused: %v", err)
		}
	})
	for name, env := range map[string][2]string{
		"unknown option":      {"SMTP_URL", "smtp://relay.corp.test:25?tls=off"},
		"smtps with option":   {"SMTP_URL", "smtps://relay.corp.test:465?insecure=1"},
		"days too high":       {"IDLE_CLEANUP_DAYS", "4000"},
		"negative days":       {"IDLE_CLEANUP_DAYS", "-1"},
		"not an smtp url":     {"SMTP_URL", "https://mail.corp.test"},
		"from not an address": {"SMTP_FROM", "nobody"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(env[0], env[1])
			if _, err := loadIdleCleanup(); err == nil {
				t.Fatalf("%s=%s accepted", env[0], env[1])
			}
		})
	}
}
