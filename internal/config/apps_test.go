package config

import "testing"

func TestMCPAppFlagGrammar(t *testing.T) {
	for _, value := range []string{"off", "on", "accounts:alice", "accounts:alice, bob"} {
		if !validAppFlag(value) {
			t.Errorf("valid flag refused: %q", value)
		}
	}
	for _, value := range []string{"", "true", "ON", "accounts:", "accounts:alice,", "accounts: ,bob"} {
		if validAppFlag(value) {
			t.Errorf("invalid flag accepted: %q", value)
		}
	}
}
