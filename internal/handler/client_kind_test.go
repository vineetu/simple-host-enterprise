package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func request(userAgent string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/portfolio/", nil)
	if userAgent != "" {
		r.Header.Set("User-Agent", userAgent)
	} else {
		r.Header.Del("User-Agent")
	}
	return r
}

// Real browsers must keep counting. Failing open is the safe direction: an
// unrecognised client is treated as a reader, not silently dropped.
func TestRealBrowsersCountAsReaders(t *testing.T) {
	agents := []string{
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36 Edg/130.0.0.0",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.6 Safari/605.1.15",
		"Mozilla/5.0 (X11; Linux x86_64; rv:133.0) Gecko/20100101 Firefox/133.0",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.6 Mobile/15E148 Safari/604.1",
		"some-client-we-have-never-heard-of/1.0",
	}
	for _, agent := range agents {
		if classifyClient(request(agent)).isBot() {
			t.Errorf("classified as a bot but should count: %s", agent)
		}
	}
}

// These are the clients that inflated the counts.
func TestAgentAndScriptClientsAreNotReaders(t *testing.T) {
	agents := []string{
		"curl/8.7.1",
		"Wget/1.21.4",
		"python-requests/2.32.3",
		"Go-http-client/2.0",
		"node",
		"node-fetch/1.0",
		"axios/1.7.2",
		"okhttp/4.12.0",
		"Java/17.0.9",
		"PostmanRuntime/7.42.0",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/131.0.0.0 Safari/537.36",
		"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
		"Slackbot-LinkExpanding 1.0 (+https://api.slack.com/robots)",
		"facebookexternalhit/1.1",
	}
	for _, agent := range agents {
		if !classifyClient(request(agent)).isBot() {
			t.Errorf("counted as a reader but is not one: %s", agent)
		}
	}
}

func TestMissingUserAgentIsNotAReader(t *testing.T) {
	if !classifyClient(request("")).isBot() {
		t.Error("a request with no user agent was counted as a reader")
	}
}

// The reliable signal: our own tooling says so. This is what the skill sends
// while verifying a deploy, and it must win regardless of user agent — an agent
// driving a real browser looks exactly like a person otherwise.
func TestCheckHeaderMarksOurOwnVerification(t *testing.T) {
	r := request("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	if classifyClient(r).isBot() {
		t.Fatal("precondition: this user agent should look like a reader")
	}
	r.Header.Set(CheckHeader, "1")
	if !classifyClient(r).isBot() {
		t.Error("the deploy-check header was ignored")
	}
}

// Case should not decide whether someone is counted.
func TestClassificationIsCaseInsensitive(t *testing.T) {
	for _, agent := range []string{"CURL/8.7.1", "Go-HTTP-Client/2.0", "GoogleBot/2.1"} {
		if !classifyClient(request(agent)).isBot() {
			t.Errorf("case changed the verdict: %s", agent)
		}
	}
}
