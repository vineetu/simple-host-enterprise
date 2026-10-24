package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/vsriram/simple-host/internal/db"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const notifyTestSecret = "whsec_MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

type receivedNotification struct {
	header    http.Header
	body      map[string]any
	duplicate bool
}
type notificationReceiver struct {
	server      *httptest.Server
	mu          sync.Mutex
	keys        []string
	seen        map[string]bool
	requests    []receivedNotification
	status      int
	echo        bool
	reflectBody bool
}

func newNotificationReceiver(t *testing.T) *notificationReceiver {
	t.Helper()
	x := &notificationReceiver{keys: []string{notifyTestSecret}, seen: map[string]bool{}, status: 200, echo: true}
	x.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		x.mu.Lock()
		defer x.mu.Unlock()
		id, ts := r.Header.Get("webhook-id"), r.Header.Get("webhook-timestamp")
		seconds, err := strconv.ParseInt(ts, 10, 64)
		if id == "" || err != nil || time.Since(time.Unix(seconds, 0)).Abs() > 5*time.Minute {
			t.Error("missing id or stale timestamp")
		}
		// Independent Standard Webhooks verifier: decode the receiver's keys and
		// compare each advertised v1 entry with HMAC(id.timestamp.body).
		for _, secret := range x.keys {
			key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
			if err != nil {
				t.Error(err)
			}
			mac := hmac.New(sha256.New, key)
			_, _ = fmt.Fprintf(mac, "%s.%s.", id, ts)
			mac.Write(body)
			valid := false
			for _, entry := range strings.Fields(r.Header.Get("webhook-signature")) {
				version, encoded, ok := strings.Cut(entry, ",")
				signature, e := base64.StdEncoding.DecodeString(encoded)
				if ok && version == "v1" && e == nil && hmac.Equal(mac.Sum(nil), signature) {
					valid = true
				}
			}
			if !valid {
				t.Error("signature did not verify")
			}
		}
		var payload map[string]any
		if json.Unmarshal(body, &payload) != nil {
			t.Error("invalid event JSON")
		}
		if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("User-Agent") != "SimpleHost-Webhooks/1" {
			t.Error("missing webhook headers")
		}
		x.requests = append(x.requests, receivedNotification{r.Header.Clone(), payload, x.seen[id]})
		x.seen[id] = true
		if x.status == 302 {
			w.Header().Set("Location", x.server.URL+"/redirected")
		}
		w.WriteHeader(x.status)
		if x.reflectBody {
			_, _ = w.Write(body)
		} else if payload["type"] == "verification" && x.echo {
			_ = json.NewEncoder(w).Encode(map[string]any{"challenge": payload["challenge"]})
		} else {
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(x.server.Close)
	return x
}
func (x *notificationReceiver) client() notifyClient {
	roots := x509.NewCertPool()
	roots.AddCert(x.server.Certificate())
	return notifyClient{allowLoopback: true, roots: roots}
}
func (x *notificationReceiver) snapshot() []receivedNotification {
	x.mu.Lock()
	defer x.mu.Unlock()
	return append([]receivedNotification(nil), x.requests...)
}
func (x *notificationReceiver) setStatus(status int) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.status = status
}
func TestNotifyClientRefusesAddresses(t *testing.T) {
	c := notifyClient{}
	for _, raw := range []string{"https://127.0.0.1/", "https://169.254.169.254/", "https://10.0.0.1/", "https://[::1]/", "https://[::ffff:127.0.0.1]/", "https://100.64.0.1/", "https://[fc00::1]/"} {
		if _, err := c.validate(context.Background(), raw); !errors.Is(err, notifyFailure("refused_address")) {
			t.Errorf("%s: %v", raw, err)
		}
	}
	for _, raw := range []string{"http://example.com/", "https://user:pass@example.com/", "https://example.com/#fragment", "https://example.com/#", "https://example.com:0/", strings.Repeat("a", 2049)} {
		if _, err := c.validate(context.Background(), raw); !errors.Is(err, notifyFailure("invalid_url")) {
			t.Errorf("invalid URL accepted: %s", raw)
		}
	}
	c.resolver = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("10.0.0.1")}}, nil
	}
	if _, err := c.validate(context.Background(), "https://mixed.example/"); !errors.Is(err, notifyFailure("refused_address")) {
		t.Fatal("mixed answer accepted")
	}
	calls := 0
	c.resolver = func(context.Context, string) ([]net.IPAddr, error) {
		calls++
		ip := "8.8.8.8"
		if calls > 1 {
			ip = "127.0.0.1"
		}
		return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
	}
	result := c.send(context.Background(), db.Notification{URL: "https://rebind.example/", Secret: notifyTestSecret}, "evt_test", []byte(`{}`))
	if result.Result != "refused_address" || calls != 2 {
		t.Fatalf("rebinding: %s, lookups %d", result.Result, calls)
	}
	c.resolver = func(context.Context, string) ([]net.IPAddr, error) { return nil, errors.New("lookup failed") }
	if _, err := c.validate(context.Background(), "https://dns.example/"); !errors.Is(err, notifyFailure("dns_error")) {
		t.Fatal(err)
	}
}
func TestNotifyClientRedirectAndHeaders(t *testing.T) {
	x := newNotificationReceiver(t)
	c := x.client()
	x.setStatus(302)
	n := db.Notification{ID: "sub_test", URL: x.server.URL, Secret: notifyTestSecret, Source: "owner", AuthHeaderName: "Authorization", AuthHeaderValue: "Bearer receiver"}
	result := c.send(context.Background(), n, "evt_redirect", []byte(`{}`))
	if result.Result != "redirect" || result.Status != 302 {
		t.Fatal(result.Result)
	}
	requests := x.snapshot()
	if len(requests) != 1 {
		t.Fatal("followed redirect")
	}
	r := requests[0]
	if r.header.Get("svix-signature") != r.header.Get("webhook-signature") || r.header.Get("Authorization") != "Bearer receiver" {
		t.Fatal("REST headers missing")
	}
	n.Source = "mcp"
	x.setStatus(200)
	result = c.send(context.Background(), n, "evt_mcp", []byte(`{}`))
	if !result.Delivered {
		t.Fatal(result.Result)
	}
	requests = x.snapshot()
	if requests[1].header.Get("svix-id") != "" || requests[1].header.Get("Authorization") != "" {
		t.Fatal("owner headers sent on MCP")
	}
	c.roots = nil
	result = c.send(context.Background(), n, "evt_tls", []byte(`{}`))
	if result.Result != "tls_error" {
		t.Fatalf("untrusted TLS: %s", result.Result)
	}
}
func TestNotifyHeaderAndSecretRules(t *testing.T) {
	for _, name := range []string{"Host", "Content-Type", "Content-Length", "Connection", "Transfer-Encoding", "Cookie", "User-Agent", "Webhook-ID", "Svix-Signature", "X-MCP-Subscription-Id", "bad_name"} {
		if validNotifyHeader(name, "x") {
			t.Errorf("accepted %s", name)
		}
	}
	if validNotifyHeader("Authorization", "x\r\ny") || validNotifyHeader("Authorization", strings.Repeat("x", 1025)) || !validNotifyHeader("Authorization", "Bearer test") {
		t.Fatal("header values")
	}
	for _, secret := range []string{"no-prefix", "whsec_?", "whsec_" + base64.StdEncoding.EncodeToString(make([]byte, 23)), "whsec_" + base64.StdEncoding.EncodeToString(make([]byte, 65))} {
		if _, err := notifySecretKey(secret); err == nil {
			t.Fatal("accepted invalid secret")
		}
	}
	generated, err := newNotifySecret()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(generated, "whsec_"))
	if err != nil || len(decoded) != 32 {
		t.Fatal("generated key length")
	}
}

func TestNotifyOperatorCIDRAndEncryptedDelivery(t *testing.T) {
	x := newNotificationReceiver(t)
	c := x.client()
	c.allowLoopback = false
	c.allowed = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	h := (&SiteHandler{}).WithNotifications(bytes.Repeat([]byte{5}, 32), nil)
	secret, err := h.notifyCipher.sealNotification("sub_test", notifyTestSecret)
	if err != nil {
		t.Fatal(err)
	}
	c.secrets = &h.notifyCipher
	n := db.Notification{ID: "sub_test", URL: x.server.URL, Source: "owner", Secret: secret}
	if result := c.send(context.Background(), n, "evt_allowed", []byte(`{}`)); !result.Delivered {
		t.Fatal("allowlisted receiver refused", result.Result)
	}
	for _, raw := range []string{"https://127.0.0.2/", "https://10.0.0.1/", "https://169.254.169.254/", "https://[::1]/", "https://[64:ff9b::a00:1]/", "https://[2002:a00:1::1]/"} {
		if _, err := c.validate(context.Background(), raw); err == nil {
			t.Fatal("address outside CIDR admitted")
		}
	}
	c.secrets = &notificationCipher{}
	if result := c.send(context.Background(), n, "evt_unreadable", []byte(`{}`)); result.Delivered || result.Result != "secret_unreadable" {
		t.Fatal("unreadable secret was sent")
	}
	if len(x.snapshot()) != 1 {
		t.Fatal("unreadable secret reached receiver")
	}
}
