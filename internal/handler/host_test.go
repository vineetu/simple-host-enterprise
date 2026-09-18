package handler

import "testing"

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

// TestHostModelClassify covers the three shapes: base, owner
// ("<label>.<base>"), and restricted-site ("<owner label>--<site
// label>.<base>", design.md 5.2a). "--" is reserved in owner labels, so any
// label containing it that cannot form a valid restricted-site pair (the
// owner part shorter than three characters, design.md 10.6's RFC 5890 rule)
// classifies as unknown rather than as an ordinary owner.
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
		{"alice.bob.foo.example", hostUnknown, ""},
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
		// Restricted-site shape: owner label at least 3 characters.
		{"alice--blog.foo.example", hostRestrictedSite, "alice--blog"},
		{"ali--blog.foo.example", hostRestrictedSite, "ali--blog"},
		{"ALICE--Blog.foo.example", hostRestrictedSite, "alice--blog"}, // normalizeHost lowercases first
		// Owner part too short (RFC 5890's "--" at position 3 is reserved):
		// neither an owner label (it contains "--", which is reserved) nor a
		// well-formed restricted-site label.
		{"ab--blog.foo.example", hostUnknown, ""},
		{"a--blog.foo.example", hostUnknown, ""},
		{"--blog.foo.example", hostUnknown, ""},
		// A site part that is empty or itself malformed.
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

func TestSplitRestrictedSiteLabel(t *testing.T) {
	cases := []struct {
		in        string
		wantOwner string
		wantSite  string
		wantOK    bool
	}{
		{"alice--blog", "alice", "blog", true},
		{"ali--my-blog", "ali", "my-blog", true},
		{"noseparator", "", "", false},
	}
	for _, tc := range cases {
		owner, site, ok := SplitRestrictedSiteLabel(tc.in)
		if owner != tc.wantOwner || site != tc.wantSite || ok != tc.wantOK {
			t.Errorf("SplitRestrictedSiteLabel(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.in, owner, site, ok, tc.wantOwner, tc.wantSite, tc.wantOK)
		}
	}
}

func TestRestrictedSiteAddressable(t *testing.T) {
	m := testHostModel(t)
	cases := []struct {
		owner, site string
		want        bool
	}{
		{"alice", "blog", true},
		{"ali", "blog", true},
		{"ab", "blog", false},       // owner label too short (2 chars)
		{"alice", "my site", false}, // site name does not fold to a valid label
		{"al@ice", "blog", false},   // owner name is not addressable at all
	}
	for _, tc := range cases {
		if got := m.RestrictedSiteAddressable(tc.owner, tc.site); got != tc.want {
			t.Errorf("RestrictedSiteAddressable(%q, %q) = %v, want %v", tc.owner, tc.site, got, tc.want)
		}
	}
}

func TestHostModelSiteHostAndOwnsLabel(t *testing.T) {
	m, err := NewHostModel("https://foo.example")
	if err != nil {
		t.Fatalf("NewHostModel: %v", err)
	}
	if got := m.SiteHost("Alice.Smith"); got != "alice-smith.foo.example" {
		t.Errorf("SiteHost = %q", got)
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
		{"alice--blog.safe.example", "alice--blog.safe.example"},
		{"a.b.safe.example", "safe.example"},
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

// TestHostModelSiteURL covers design.md 7.1's "site links are always the
// absolute short form": there is no long-path fallback any more, only the
// owner's own host (restricted=false) or the restricted site's own host
// (restricted=true). An owner whose name cannot be a hostname label, or a
// restricted site that cannot form its own label, gets "" rather than a
// broken address spliced from unsafe input.
func TestHostModelSiteURL(t *testing.T) {
	cases := []struct {
		name       string
		base       string
		user, site string
		restricted bool
		want       string
	}{
		{name: "owner host short address", base: "https://foo.example", user: "Alice.B", site: "my-site",
			want: "https://alice-b.foo.example/my-site/"},
		{name: "escapes the site segment", base: "https://foo.example", user: "alice", site: "100%",
			want: "https://alice.foo.example/100%25/"},
		{name: "keeps the base scheme and port", base: "http://Foo.Example:8080/", user: "alice", site: "s",
			want: "http://alice.foo.example:8080/s/"},
		{name: "an owner whose name is not a label has no address", base: "https://foo.example", user: "al@ice", site: "s",
			want: ""},
		{name: "restricted site's own host", base: "https://foo.example", user: "alice", site: "private", restricted: true,
			want: "https://alice--private.foo.example/"},
		{name: "restricted but owner label too short", base: "https://foo.example", user: "ab", site: "private", restricted: true,
			want: ""},
		{name: "restricted but site name not addressable", base: "https://foo.example", user: "alice", site: "two words", restricted: true,
			want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := NewHostModel(tc.base)
			if err != nil {
				t.Fatalf("NewHostModel: %v", err)
			}
			if got := m.SiteURL(tc.user, tc.site, tc.restricted); got != tc.want {
				t.Errorf("SiteURL = %q, want %q", got, tc.want)
			}
		})
	}

	// The zero value is what handler tests build with: no base host, so
	// there is no address to give.
	var zero HostModel
	if got := zero.SiteURL("alice", "s", false); got != "" {
		t.Errorf("zero SiteURL = %q, want \"\"", got)
	}
}

// OwnerOrigin rebuilds an owner host's origin from the configured base URL's
// scheme and port. It is what the Origin check compares against on an owner
// host, and what shortSiteURL is built on, so the two can never disagree.
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
		// The short site URL is the same origin plus the site path, so a
		// change to one cannot silently diverge from the other.
		if got, want := m.SiteURL(test.label, "demo", false), test.want+"/demo/"; got != want {
			t.Errorf("SiteURL on %q = %q, want %q", test.base, got, want)
		}
	}
}
