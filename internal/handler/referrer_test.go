package handler

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReferrerDomain(t *testing.T) {
	const base = "hosting.corp.test"
	cases := []struct{ host, referer, want string }{
		{"demo.alice." + base, "", ""},
		{"demo.alice." + base, "https://news.example.com/item?id=42&token=secret", "news.example.com"},
		{"demo.alice." + base, "https://NEWS.Example.com:8443/x", "news.example.com"},
		{"demo.alice." + base, "https://demo.alice." + base + "/other.html", ""},
		{"demo.alice." + base + ":8443", "https://demo.alice." + base + ":8443/", ""},
		{"demo.alice." + base, "https://secret-project.bob." + base + "/", "*." + base},
		{"demo.alice." + base, "https://" + base + "/search?q=demo", base},
		{"demo.alice." + base, "android-app://com.slack/", ""},
		{"demo.alice." + base, "not a url at all", ""},
		{"demo.alice." + base, "https://bücher.example/", "xn--bcher-kva.example"},
		{"demo.alice." + base, "https://münchen.de/", "xn--mnchen-3ya.de"},
		{"demo.alice." + base, "https://例え.テスト/", "xn--r8jz45g.xn--zckzah"},
		{"demo.alice." + base, "https://a<b>/", otherReferrer},
		{"demo.alice." + base, "https://a'\"b/", otherReferrer},
		{"demo.alice." + base, "https://a;b,c(d)/", otherReferrer},
		{"demo.alice." + base, "https://evil%E2%80%AE.com/", otherReferrer},
		{"demo.alice." + base, "https://10.1.2.3/", otherReferrer},
		{"demo.alice." + base, "https://[::1]:8080/", otherReferrer},
		{"demo.alice." + base, "https://" + strings.Repeat("a.", 130) + "com/", otherReferrer},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "https://"+c.host+"/", nil)
		r.Host = c.host
		if c.referer != "" {
			r.Header.Set("Referer", c.referer)
		}
		if got := referrerDomain(r, base); got != c.want {
			t.Errorf("referrerDomain(host %q, referer %q) = %q, want %q", c.host, c.referer, got, c.want)
		}
	}
}
