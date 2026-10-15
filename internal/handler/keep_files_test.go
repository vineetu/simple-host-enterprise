package handler

import (
	"net/http/httptest"
	"testing"
)

func TestDeployKeepQuery(t *testing.T) {
	for _, tc := range []struct {
		query  string
		keep   bool
		remove []string
		ok     bool
	}{
		{"", false, nil, true},
		{"keep=1", true, nil, true},
		{"keep=1&remove=old.html&remove=./img/a.png", true, []string{"old.html", "img/a.png"}, true},
		{"remove=old.html", false, nil, false},
		{"keep=yes", false, nil, false},
		{"keep=1&remove=../etc/passwd", false, nil, false},
		{"keep=1&remove=/abs.html", false, nil, false},
	} {
		rec := httptest.NewRecorder()
		keep, remove, ok := deployKeep(rec, httptest.NewRequest("PUT", "/api/sites/demo?"+tc.query, nil))
		if ok != tc.ok || keep != tc.keep {
			t.Errorf("%q: keep=%v ok=%v, want %v %v", tc.query, keep, ok, tc.keep, tc.ok)
			continue
		}
		for _, p := range tc.remove {
			if !remove[p] {
				t.Errorf("%q: %s not in remove %v", tc.query, p, remove)
			}
		}
		if !ok && rec.Code != 400 {
			t.Errorf("%q: status %d, want 400", tc.query, rec.Code)
		}
	}
}
