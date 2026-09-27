package handler

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// maxReferrerDomainLen is the longest DNS name; anything longer is not one.
const maxReferrerDomainLen = 253

// referrerDomain is the host of the page that linked to r, for the access
// log's "where visitors came from": the host only, never the Referer's path
// or query. "" for no Referer, a non-http(s) one, or one from the same host
// (moving between the site's own pages). A page elsewhere on this install
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
	if host == "" || len(host) > maxReferrerDomainLen {
		return ""
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
