package handler

import (
	"net/http"
	"strings"
)

// CheckHeader is set by our own tooling when it fetches a deployed site purely
// to confirm the deploy worked.
//
// This exists because the simple-host skill instructs every agent to fetch the
// canonical URL after every deploy ("Completion standard"). That verification
// traffic is the single largest source of non-human views on the platform, and
// it is traffic we ask for — so rather than trying to detect it after the fact,
// the skill announces it and the server takes it at its word. Any client may
// send it; the worst a caller can do is decline to be counted.
const CheckHeader = "X-Simple-Host-Check"

// clientKind is how a request is counted. Traffic is never discarded — a bot
// pageview is recorded in its own column so a number can always be explained,
// and so a misclassification is visible rather than silent.
type clientKind int

const (
	clientHuman clientKind = iota
	clientBot
)

func (k clientKind) isBot() bool { return k == clientBot }

// String is access_log's client_kind column, a plain string
// so internal/audit (which must not depend on this package) can accept it.
func (k clientKind) String() string {
	if k == clientBot {
		return "bot"
	}
	return "human"
}

// nonBrowserAgents are substrings (lowercased) of user agents that do not
// belong to somebody reading a page. Two groups: HTTP clients that agents and
// scripts use, and browsers driven by automation.
//
// This list is a supplement, not the mechanism. It is a denylist, so it is
// permanently incomplete — anything it misses is counted as human, which is the
// safe direction to fail. The reliable signal is CheckHeader above.
var nonBrowserAgents = []string{
	// HTTP clients — what an agent or script reaches for.
	"curl/", "wget/", "python-requests", "python-urllib", "httpie",
	"go-http-client", "okhttp", "axios/", "node-fetch", "undici",
	"java/", "libwww-perl", "ruby", "guzzle", "postman",
	// Driven browsers.
	"headlesschrome", "phantomjs", "electron/", "playwright", "puppeteer",
	"selenium", "webdriver", "cypress",
	// Crawlers and link unfurlers.
	"bot", "crawler", "spider", "slurp", "facebookexternalhit",
	"embedly", "quora link preview", "outbrain", "pinterest/",
	"vkshare", "w3c_validator", "whatsapp", "flipboard", "tumblr",
	"bitlybot", "skypeuripreview", "nuzzel", "discord", "google-read-aloud",
	"preview", "monitor", "uptime", "pingdom", "statuscake",
}

// bareNonBrowserAgents are user agents matched in full rather than by
// substring. Node's fetch, for one, sends exactly "node" — too short a token to
// match as a substring without catching innocent bystanders.
var bareNonBrowserAgents = map[string]bool{
	"node": true, "java": true, "python": true, "go": true, "ruby": true,
	"http": true, "unknown": true, "-": true,
}

// classifyClient decides how a request should be counted.
func classifyClient(r *http.Request) clientKind {
	// Our own tooling saying "this is a deploy check, not a reader".
	if r.Header.Get(CheckHeader) != "" {
		return clientBot
	}

	agent := strings.ToLower(strings.TrimSpace(r.Header.Get("User-Agent")))

	// No user agent at all is never a browser.
	if agent == "" {
		return clientBot
	}

	if bareNonBrowserAgents[agent] {
		return clientBot
	}

	for _, marker := range nonBrowserAgents {
		if strings.Contains(agent, marker) {
			return clientBot
		}
	}

	return clientHuman
}
