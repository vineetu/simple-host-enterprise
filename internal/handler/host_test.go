package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestOwnerLabel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Alice.Smith", "alice-smith"},
		{"alice", "alice"},
		{"Alice", "alice"},
		{"a.b.c", "a-b-c"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := ownerLabel(tc.in); got != tc.want {
			t.Errorf("ownerLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSiteLabel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"My.Site", "my-site"},
		{"blog", "blog"},
		{"two words", "two words"}, // folded but not sanitized: isValidLabel still refuses the space
		{"", ""},
	}
	for _, tc := range cases {
		if got := siteLabel(tc.in); got != tc.want {
			t.Errorf("siteLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIsValidLabel(t *testing.T) {
	long := make([]byte, 64)
	for i := range long {
		long[i] = 'a'
	}
	cases := []struct {
		in   string
		want bool
	}{
		{"alice", true},
		{"alice-smith", true},
		{"a1", true},
		{"1", true},
		{string(long[:63]), true},
		{"", false},
		{"Alice", false},
		{"-x", false},
		{"x-", false},
		{"a.b", false},
		{"a_b", false},
		{"a b", false},
		{string(long), false},
	}
	for _, tc := range cases {
		if got := isValidLabel(tc.in); got != tc.want {
			t.Errorf("isValidLabel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"Foo.Example", "foo.example"},
		{"foo.example:8080", "foo.example"},
		{"foo.example.", "foo.example"},
		{"Foo.Example.:443", "foo.example"},
		{"Koo.example", ""},                    // Kelvin sign would fold to "k"; must not become a label
		{"[alice.example]", "[alice.example]"}, // brackets unwrap only around an IP literal
		{"[alice.example]:443", ""},            // and with a port the spelling is refused outright
		{"[::1]:8080", "::1"},
		{"[::1]", "::1"},
		{"10.0.0.5:8080", "10.0.0.5"},
		{"localhost", "localhost"},
	}
	for _, tc := range cases {
		if got := normalizeHost(tc.in); got != tc.want {
			t.Errorf("normalizeHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNewHostModel(t *testing.T) {
	m, err := NewHostModel("https://Foo.Example:8443/")
	if err != nil {
		t.Fatalf("NewHostModel: %v", err)
	}
	if got := m.BaseHost(); got != "foo.example" {
		t.Errorf("BaseHost = %q, want foo.example", got)
	}

	m, err = NewHostModel("https://simple-host.example.com")
	if err != nil {
		t.Fatalf("NewHostModel: %v", err)
	}
	if got := m.BaseHost(); got != "simple-host.example.com" {
		t.Errorf("BaseHost = %q", got)
	}

	for _, bad := range []string{"", "/relative/path", "https://", "://bad", "not a url"} {
		if _, err := NewHostModel(bad); err == nil {
			t.Errorf("NewHostModel(%q) expected error", bad)
		}
	}
}

// TestHostModelClassify covers the shapes: base, owner ("<label>.<base>"),
// site ("<site part>.<owner label>.<base>") and the v1.2 specific-site
// address ("<owner>--<site>.<base>", which only redirects). "--" is reserved
// in owner labels; "xn--" reads as punycode and is refused in any position.
func TestHostModelClassify(t *testing.T) {
	m, err := NewHostModel("https://foo.example")
	if err != nil {
		t.Fatalf("NewHostModel: %v", err)
	}
	cases := []struct {
		in    string
		kind  hostKind
		label string
	}{
		{"foo.example", hostBase, ""},
		{"foo.example:443", hostBase, ""},
		{"FOO.EXAMPLE.", hostBase, ""},
		{"alice.foo.example", hostOwner, "alice"},
		{"alice.foo.example.", hostOwner, "alice"},
		{"Alice.foo.example:8443", hostOwner, "alice"},
		{"alice-smith.foo.example", hostOwner, "alice-smith"},
		{"-x.foo.example", hostUnknown, ""},
		{"x-.foo.example", hostUnknown, ""},
		{"xfoo.example", hostUnknown, ""},
		{".foo.example", hostUnknown, ""},
		{"10.0.0.5:8080", hostUnknown, ""},
		{"[::1]:8080", hostUnknown, ""},
		{"localhost", hostUnknown, ""},
		{"", hostUnknown, ""},
		{"evil.com", hostUnknown, ""},
		{"foo.example.evil.com", hostUnknown, ""},
		// A site's own host: two labels under the base.
		{"blog.alice.foo.example", hostSite, "blog.alice"},
		{"BLOG.Alice.foo.example", hostSite, "blog.alice"}, // normalizeHost lowercases first
		{"q3.team-sales.foo.example", hostSite, "q3.team-sales"},
		{"blog.a.foo.example", hostSite, "blog.a"},
		{"a--b.alice.foo.example", hostSite, "a--b.alice"}, // a site part may hold "--"
		// Three labels deep is nobody's.
		{"a.b.c.foo.example", hostUnknown, ""},
		// Malformed parts.
		{"-blog.alice.foo.example", hostUnknown, ""},
		{"blog.-alice.foo.example", hostUnknown, ""},
		{"blog.al--ice.foo.example", hostUnknown, ""}, // owner labels never hold "--"
		{".alice.foo.example", hostUnknown, ""},
		{"blog..foo.example", hostUnknown, ""},
		// Punycode, in either position.
		{"xn--blog.foo.example", hostUnknown, ""},
		{"xn--blog.alice.foo.example", hostUnknown, ""},
		{"blog.xn--alice.foo.example", hostUnknown, ""},
		// The v1.2 specific-site address.
		{"alice--blog.foo.example", hostLegacySite, "alice--blog"},
		{"ALICE--Blog.foo.example", hostLegacySite, "alice--blog"},
		{"team-sales--q3.foo.example", hostLegacySite, "team-sales--q3"},
		{"--blog.foo.example", hostUnknown, ""},
		{"alice--.foo.example", hostUnknown, ""},
		{"alice---blog.foo.example", hostUnknown, ""}, // splits to site "-blog", leading hyphen invalid
	}
	for _, tc := range cases {
		kind, label := m.Classify(tc.in)
		if kind != tc.kind || label != tc.label {
			t.Errorf("Classify(%q) = (%v, %q), want (%v, %q)", tc.in, kind, label, tc.kind, tc.label)
		}
	}
}

func TestSplitSiteLabel(t *testing.T) {
	cases := []struct {
		in        string
		wantSite  string
		wantOwner string
		wantOK    bool
	}{
		{"blog.alice", "blog", "alice", true},
		{"my-blog.ali", "my-blog", "ali", true},
		{"x--y.ab", "x--y", "ab", true},
		{"noseparator", "noseparator", "", false},
	}
	for _, tc := range cases {
		site, owner, ok := SplitSiteLabel(tc.in)
		if site != tc.wantSite || owner != tc.wantOwner || ok != tc.wantOK {
			t.Errorf("SplitSiteLabel(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.in, site, owner, ok, tc.wantSite, tc.wantOwner, tc.wantOK)
		}
	}
}

func TestSplitLegacySiteLabel(t *testing.T) {
	owner, site, ok := splitLegacySiteLabel("alice--x--y")
	if owner != "alice" || site != "x--y" || !ok {
		t.Errorf("splitLegacySiteLabel = (%q, %q, %v)", owner, site, ok)
	}
	if _, _, ok := splitLegacySiteLabel("alice"); ok {
		t.Error("splitLegacySiteLabel(alice) ok, want not")
	}
}

// siteHostPart keeps a name that is already a label (every name a new site
// may have, and every "specific" site's pre-v1.3 address), and gives any
// other name a deterministic, collision-resistant part. It depends on the
// name alone, never on the owner.
func TestSiteHostPart(t *testing.T) {
	long := strings.Repeat("a", 70)
	cases := []struct {
		site, want string
	}{
		{"blog", "blog"},
		{"My.Site", "my-site"},
		{"Notes", "notes"},
		{strings.Repeat("a", 63), strings.Repeat("a", 63)},
		{"two words", "two-words-" + shortHash("two words")},
		{"a_b__c", "a-b-c-" + shortHash("a_b__c")},
		{"!!!", "s-" + shortHash("!!!")},
		{long, strings.Repeat("a", 63-7) + "-" + shortHash(long)},
		{"xn--abc", "abc-" + shortHash("xn--abc")},
	}
	for _, tc := range cases {
		got := siteHostPart(tc.site)
		if got != tc.want {
			t.Errorf("siteHostPart(%q) = %q, want %q", tc.site, got, tc.want)
		}
		if !isValidLabel(got) || strings.HasPrefix(got, "xn--") {
			t.Errorf("siteHostPart(%q) = %q is not a usable label", tc.site, got)
		}
	}
}

func shortHash(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])[:6]
}

func TestValidNewSiteName(t *testing.T) {
	m := testHostModel(t)
	longOwner := strings.Repeat("o", 63)
	cases := []struct {
		owner, site string
		want        bool
	}{
		{"alice", "blog", true},
		{"ab", "blog", true},
		{"team-sales", "q3-plan", true},
		{"alice", "Blog", false},
		{"alice", "my.site", false},
		{"alice", "my site", false},
		{"alice", "-x", false},
		{"alice", "xn--blog", false},
		{"alice", strings.Repeat("a", 63), true},
		{"alice", strings.Repeat("a", 64), false},
		// The owner's length no longer shortens the name.
		{longOwner, strings.Repeat("a", 63), true},
	}
	for _, tc := range cases {
		if got := m.ValidNewSiteName(tc.owner, tc.site); got != tc.want {
			t.Errorf("ValidNewSiteName(%q, %q) = %v, want %v", tc.owner, tc.site, got, tc.want)
		}
	}
	if got := MaxSiteNameLen("alice"); got != 63 {
		t.Errorf("MaxSiteNameLen(alice) = %d, want 63", got)
	}

	// A host longer than DNS allows is refused even for a valid name.
	long, err := NewHostModel("https://" + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + ".example")
	if err != nil {
		t.Fatal(err)
	}
	if long.ValidNewSiteName(longOwner, strings.Repeat("a", 63)) {
		t.Error("ValidNewSiteName accepted a host over 253 characters")
	}
	if !long.ValidNewSiteName("alice", "blog") {
		t.Error("ValidNewSiteName refused a short host under a long base")
	}
}

func TestHostModelHostsAndOwnsLabel(t *testing.T) {
	m, err := NewHostModel("https://foo.example")
	if err != nil {
		t.Fatalf("NewHostModel: %v", err)
	}
	if got := m.OwnerHost("Alice.Smith"); got != "alice-smith.foo.example" {
		t.Errorf("OwnerHost = %q", got)
	}
	if got := m.SiteHost("Alice.Smith", "blog"); got != "blog.alice-smith.foo.example" {
		t.Errorf("SiteHost = %q", got)
	}
	if got := m.SiteHost("al@ice", "blog"); got != "" {
		t.Errorf("SiteHost(al@ice) = %q, want none", got)
	}
	if !m.OwnsLabel("alice-smith", "Alice.Smith") {
		t.Error("OwnsLabel should match derived label")
	}
	if m.OwnsLabel("Alice-Sriram", "Alice.Smith") {
		t.Error("OwnsLabel is a pure comparison; label is not normalized")
	}
	if m.OwnsLabel("alice", "bob") {
		t.Error("OwnsLabel should not match a different user")
	}
}

func TestHostModelRedirectHost(t *testing.T) {
	m, err := NewHostModel("https://safe.example")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ in, want string }{
		{"safe.example", "safe.example"},
		{"safe.example:8443", "safe.example"},
		{"alice.safe.example", "alice.safe.example"},
		{"Alice.Safe.Example.", "alice.safe.example"},
		{"alice.safe.example:8080", "alice.safe.example"},
		{"blog.alice.safe.example", "blog.alice.safe.example"},
		{"alice--blog.safe.example", "alice--blog.safe.example"},
		{"a.b.c.safe.example", "safe.example"},
		{"-x.safe.example", "safe.example"},
		{"10.0.0.5:8080", "safe.example"},
		{"evil.com", "safe.example"},
		{"safe.example.evil.com", "safe.example"},
		{"", "safe.example"},
	}
	for _, tc := range cases {
		if got := m.RedirectHost(tc.in); got != tc.want {
			t.Errorf("RedirectHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestHostModelSiteURL: once the owner is ready a site's address is the
// root of its own host; until then it is "<owner>.<base>/<site>/". An owner
// whose name cannot be a hostname label gets "" rather than a broken
// address spliced from unsafe input.
func TestHostModelSiteURL(t *testing.T) {
	cases := []struct {
		name       string
		base       string
		user, site string
		want       string
	}{
		{name: "own host", base: "https://foo.example", user: "Alice.B", site: "my-site",
			want: "https://my-site.alice-b.foo.example/"},
		{name: "keeps the base scheme and port", base: "http://Foo.Example:8080/", user: "alice", site: "s",
			want: "http://s.alice.foo.example:8080/"},
		{name: "short owner", base: "https://foo.example", user: "ab", site: "private",
			want: "https://private.ab.foo.example/"},
		{name: "pre-v1.3 name that is not a label", base: "https://foo.example", user: "alice", site: "two words",
			want: "https://two-words-" + shortHash("two words") + ".alice.foo.example/"},
		{name: "an owner whose name is not a label has no address", base: "https://foo.example", user: "al@ice", site: "s",
			want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := NewHostModel(tc.base)
			if err != nil {
				t.Fatalf("NewHostModel: %v", err)
			}
			if got := m.SiteURL(tc.user, tc.site); got != tc.want {
				t.Errorf("SiteURL = %q, want %q", got, tc.want)
			}
		})
	}

	// The zero value is what handler tests build with: no base host, so
	// there is no address to give.
	var zero HostModel
	if got := zero.SiteURL("alice", "s"); got != "" {
		t.Errorf("zero SiteURL = %q, want \"\"", got)
	}
}

// Until an owner's certificate is ready, SiteURL and SiteHostResolver point
// at the owner host; once it is, at the site's own host. Readiness is asked
// per owner label.
func TestHostModelOwnerReadiness(t *testing.T) {
	base := newTestHostModel(t, "https://foo.example")
	if !base.OwnerReady("anyone") {
		t.Error("no readiness func: every owner should be ready")
	}
	var asked []string
	m := base.WithOwnerReadiness(func(label string) bool {
		asked = append(asked, label)
		return label == "ready-one"
	})
	for _, tc := range []struct{ user, site, wantURL, wantHost string }{
		{"Ready.One", "blog", "https://blog.ready-one.foo.example/", "blog.ready-one.foo.example"},
		{"alice", "blog", "https://alice.foo.example/blog/", "alice.foo.example"},
		{"alice", "two words", "https://alice.foo.example/two%20words/", "alice.foo.example"},
	} {
		if got := m.SiteURL(tc.user, tc.site); got != tc.wantURL {
			t.Errorf("SiteURL(%q, %q) = %q, want %q", tc.user, tc.site, got, tc.wantURL)
		}
		got, err := m.SiteHostResolver()(context.Background(), tc.user, tc.site)
		if err != nil || got != tc.wantHost {
			t.Errorf("SiteHostResolver(%q, %q) = %q, %v, want %q", tc.user, tc.site, got, err, tc.wantHost)
		}
	}
	if len(asked) == 0 || asked[0] != "ready-one" {
		t.Errorf("readiness asked with %v, want owner labels", asked)
	}
	if _, err := m.SiteHostResolver()(context.Background(), "al@ice", "s"); err == nil {
		t.Error("SiteHostResolver for an unaddressable owner: want an error")
	}
	// The original model is unchanged: HostModel is a value.
	if !base.OwnerReady("alice") {
		t.Error("WithOwnerReadiness changed the model it was called on")
	}
}

// OwnerOrigin rebuilds an owner or site host's origin from the configured
// base URL's scheme and port. It is what the Origin check compares against,
// and what SiteURL is built on, so the two can never disagree.
func TestOwnerOrigin(t *testing.T) {
	for _, test := range []struct {
		base  string
		label string
		want  string
	}{
		{base: "https://foo.example", label: "alice", want: "https://alice.foo.example"},
		{base: "https://Foo.Example:8443/", label: "alice-b", want: "https://alice-b.foo.example:8443"},
		{base: "http://localhost:8080", label: "alice", want: "http://alice.localhost:8080"},
	} {
		m := newTestHostModel(t, test.base)
		if got := m.OwnerOrigin(test.label); got != test.want {
			t.Errorf("OwnerOrigin(%q) on %q = %q, want %q", test.label, test.base, got, test.want)
		}
		if got, want := m.SiteURL(test.label, "demo"), m.OwnerOrigin("demo."+test.label)+"/"; got != want {
			t.Errorf("SiteURL on %q = %q, want %q", test.base, got, want)
		}
	}
}
