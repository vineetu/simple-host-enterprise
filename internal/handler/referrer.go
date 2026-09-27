package handler

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxReferrerDomainLen is the longest DNS name; anything longer is not one.
const maxReferrerDomainLen = 253

// otherReferrer is recorded for a Referer whose host is not a DNS name
// (an IP address, or characters no hostname has): anyone can send any
// Referer, and the domain reaches the dashboard and AI agents
// (site_activity), so only hostname characters are ever kept.
const otherReferrer = "(other)"

// referrerHostChars is every character a hostname has, after punycode.
var referrerHostChars = regexp.MustCompile(`^[a-z0-9.-]+$`)

// referrerDomain is the host of the page that linked to r, for the access
// log's "where visitors came from": the host only, never the Referer's path
// or query. "" for no Referer, a non-http(s) one, or one from the same host
// (moving between the site's own pages); otherReferrer for a host that is
// not a DNS name; punycode for an internationalised one. A page elsewhere on this install
// is recorded as base (the base host: search, the showcase, the dashboard)
// or "*."+base (any owner or site host), so a site's name never reaches
// another owner through their referrer list.
func referrerDomain(r *http.Request, base string) string {
	raw := r.Header.Get("Referer")
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return ""
	}
	host, err = punycodeHost(host)
	if err != nil || len(host) > maxReferrerDomainLen || !referrerHostChars.MatchString(host) || net.ParseIP(host) != nil {
		return otherReferrer
	}
	self := r.Host
	if h, _, err := net.SplitHostPort(self); err == nil {
		self = h
	}
	if host == strings.ToLower(self) {
		return ""
	}
	if base != "" {
		base = strings.ToLower(base)
		if host == base {
			return base
		}
		if strings.HasSuffix(host, "."+base) {
			return "*." + base
		}
	}
	return host
}

var errPunycode = errors.New("not an internationalised domain name")

// punycodeHost writes each non-ASCII label of host as "xn--" and its
// punycode (RFC 3492), the form DNS carries. host is already lower-cased;
// no further normalisation is done. A label with anything but letters,
// digits and combining marks of some script (a direction override, a
// format character) is refused.
func punycodeHost(host string) (string, error) {
	if !utf8.ValidString(host) {
		return "", errPunycode
	}
	labels := strings.Split(host, ".")
	for i, label := range labels {
		ascii := true
		for _, r := range label {
			if r >= 0x80 {
				ascii = false
				if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.Is(unicode.Mn, r) && !unicode.Is(unicode.Mc, r) {
					return "", errPunycode
				}
			}
		}
		if ascii {
			continue
		}
		encoded, err := punycodeEncode(label)
		if err != nil {
			return "", err
		}
		labels[i] = "xn--" + encoded
	}
	return strings.Join(labels, "."), nil
}

// punycodeEncode is RFC 3492's encoder with its fixed parameters.
func punycodeEncode(s string) (string, error) {
	const (
		base, tmin, tmax, skew, damp = 36, 1, 26, 38, 700
		initialBias, initialN        = 72, 128
		maxInt                       = 1<<31 - 1
	)
	runes := []rune(s)
	var out []byte
	for _, r := range runes {
		if r < 0x80 {
			out = append(out, byte(r))
		}
	}
	b := len(out)
	h := b
	if b > 0 {
		out = append(out, '-')
	}
	digit := func(d int) byte {
		if d < 26 {
			return byte('a' + d)
		}
		return byte('0' + d - 26)
	}
	adapt := func(delta, numPoints int, first bool) int {
		if first {
			delta /= damp
		} else {
			delta /= 2
		}
		delta += delta / numPoints
		k := 0
		for delta > ((base-tmin)*tmax)/2 {
			delta /= base - tmin
			k += base
		}
		return k + (base-tmin+1)*delta/(delta+skew)
	}
	n, delta, bias := initialN, 0, initialBias
	for h < len(runes) {
		m := maxInt
		for _, r := range runes {
			if int(r) >= n && int(r) < m {
				m = int(r)
			}
		}
		if (m-n)*(h+1) > maxInt-delta {
			return "", errPunycode
		}
		delta += (m - n) * (h + 1)
		n = m
		for _, r := range runes {
			if int(r) < n {
				delta++
			}
			if int(r) != n {
				continue
			}
			q := delta
			for k := base; ; k += base {
				t := min(max(k-bias, tmin), tmax)
				if q < t {
					break
				}
				out = append(out, digit(t+(q-t)%(base-t)))
				q = (q - t) / (base - t)
			}
			out = append(out, digit(q))
			bias = adapt(delta, h+1, h == b)
			delta = 0
			h++
		}
		delta++
		n++
	}
	return string(out), nil
}
