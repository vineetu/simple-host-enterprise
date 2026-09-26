package handler

import "testing"

func TestCheckNewSiteName(t *testing.T) {
	m, err := NewHostModel("https://simple-host.example.com")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		existing []string
		ok       bool
	}{
		{"notes", nil, true},
		{"notes", []string{"notes"}, true}, // restoring into itself is not a clash
		{"Notes", nil, false},
		{"a--b", nil, true}, // a valid label; the -- refusal is for owner labels
		{"xn--abc", nil, false},
		{"-notes", nil, false},
		{"my_site", nil, false},
		{"api", nil, false},
		{"sites", nil, false},
		{string(make([]byte, 64)), nil, false},
		{"notes", []string{"Notes"}, false},
	}
	for _, c := range cases {
		err := m.CheckNewSiteName("alice", c.name, c.existing)
		if (err == nil) != c.ok {
			t.Errorf("CheckNewSiteName(%q, %v) = %v, want ok=%v", c.name, c.existing, err, c.ok)
		}
	}
}
