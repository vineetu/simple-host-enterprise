package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/oplimits"
)

type notifyFailure string

func (e notifyFailure) Error() string { return string(e) }

type notifyClient struct {
	secrets       *notificationCipher
	allowed       []netip.Prefix
	resolver      func(context.Context, string) ([]net.IPAddr, error)
	allowLoopback bool
	roots         *x509.CertPool
}

func notifySecretKey(secret string) ([]byte, error) {
	if !strings.HasPrefix(secret, "whsec_") {
		return nil, errors.New("invalid_secret")
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil || len(key) < 24 || len(key) > 64 {
		return nil, errors.New("invalid_secret")
	}
	return key, nil
}
func newNotifySecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "whsec_" + base64.StdEncoding.EncodeToString(b), nil
}
func notifySignature(secret, id, ts string, body []byte) (string, error) {
	key, err := notifySecretKey(secret)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}
func (c *notifyClient) public(ip string) bool {
	addr, err := netip.ParseAddr(ip)
	if err == nil {
		for _, prefix := range c.allowed {
			if prefix.Contains(addr.Unmap()) {
				return true
			}
		}
	}
	return isPublicIP(ip) || (c.allowLoopback && net.ParseIP(ip) != nil && net.ParseIP(ip).IsLoopback())
}
func (c *notifyClient) addresses(ctx context.Context, host string) ([]net.IPAddr, error) {
	var addrs []net.IPAddr
	if ip := net.ParseIP(host); ip != nil {
		addrs = []net.IPAddr{{IP: ip}}
	} else {
		resolver := c.resolver
		if resolver == nil {
			resolver = net.DefaultResolver.LookupIPAddr
		}
		var err error
		addrs, err = resolver(ctx, host)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return nil, notifyFailure("timeout")
			}
			return nil, notifyFailure("dns_error")
		}
	}
	if len(addrs) == 0 {
		return nil, notifyFailure("dns_error")
	}
	for _, a := range addrs {
		if a.Zone != "" || !c.public(a.IP.String()) {
			return nil, notifyFailure("refused_address")
		}
	}
	return addrs, nil
}
func (c *notifyClient) validate(ctx context.Context, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || strings.Contains(raw, "#") || u.Opaque != "" {
		return nil, notifyFailure("invalid_url")
	}
	if p := u.Port(); p != "" {
		n, e := strconv.Atoi(p)
		if e != nil || n < 1 || n > 65535 {
			return nil, notifyFailure("invalid_url")
		}
	}
	if _, err = c.addresses(ctx, u.Hostname()); err != nil {
		return nil, err
	}
	return u, nil
}
func (c *notifyClient) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, notifyFailure("refused_address")
	}
	addrs, err := c.addresses(ctx, host)
	if err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: 2 * time.Second, Control: func(network, address string, raw syscall.RawConn) error {
		ip, _, err := net.SplitHostPort(address)
		if err != nil || !c.public(ip) {
			return notifyFailure("refused_address")
		}
		return nil
	}}
	for _, a := range addrs {
		conn, e := d.DialContext(ctx, network, net.JoinHostPort(a.IP.String(), port))
		if e != nil {
			err = e
			continue
		}
		remote, _, e := net.SplitHostPort(conn.RemoteAddr().String())
		if e != nil || !c.public(remote) {
			conn.Close()
			return nil, notifyFailure("refused_address")
		}
		return conn, nil
	}
	return nil, err
}
func notifyCategory(err error) string {
	var failure notifyFailure
	if errors.As(err, &failure) {
		return string(failure)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	var ce *tls.CertificateVerificationError
	var re tls.RecordHeaderError
	if errors.As(err, &ce) || errors.As(err, &re) || strings.Contains(strings.ToLower(err.Error()), "tls") {
		return "tls_error"
	}
	return "connection_refused"
}

type notifyResult struct {
	Delivered bool   `json:"delivered"`
	Status    int    `json:"status"`
	Result    string `json:"result"`
	body      []byte
}

func (c *notifyClient) send(ctx context.Context, n db.Notification, id string, body []byte) notifyResult {
	ctx, cancel := context.WithTimeout(ctx, oplimits.Get().NotifyTimeout)
	defer cancel()
	if _, err := c.validate(ctx, n.URL); err != nil {
		return notifyResult{Result: notifyCategory(err)}
	}
	secrets := c.secrets
	if secrets == nil {
		secrets = &notificationCipher{}
	}
	secret, err := secrets.openNotification(n.ID, n.Secret)
	if err != nil {
		return notifyResult{Result: "secret_unreadable"}
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig, err := notifySignature(secret, id, ts, body)
	if err != nil {
		return notifyResult{Result: "secret_unreadable"}
	}
	if n.PrevSecret != "" && n.PrevSecretUntil != nil && time.Now().Before(*n.PrevSecretUntil) {
		previous, err := secrets.openNotification(n.ID, n.PrevSecret)
		if err != nil {
			return notifyResult{Result: "secret_unreadable"}
		}
		old, err := notifySignature(previous, id, ts, body)
		if err != nil {
			return notifyResult{Result: "secret_unreadable"}
		}
		sig += " " + old
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, bytes.NewReader(body))
	if err != nil {
		return notifyResult{Result: "connection_refused"}
	}
	if n.Source == "owner" && n.AuthHeaderName != "" {
		header, err := secrets.openNotification(n.ID+":header", n.AuthHeaderValue)
		if err != nil {
			return notifyResult{Result: "secret_unreadable"}
		}
		req.Header.Set(n.AuthHeaderName, header)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "SimpleHost-Webhooks/1")
	req.Header.Set("webhook-id", id)
	req.Header.Set("webhook-timestamp", ts)
	req.Header.Set("webhook-signature", sig)
	req.Header.Set("X-MCP-Subscription-Id", n.ID)
	if n.Source == "owner" {
		req.Header.Set("svix-id", id)
		req.Header.Set("svix-timestamp", ts)
		req.Header.Set("svix-signature", sig)
	}
	transport := &http.Transport{Proxy: nil, DialContext: c.dial, TLSClientConfig: &tls.Config{RootCAs: c.roots, MinVersion: tls.VersionTLS12}, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport, Timeout: oplimits.Get().NotifyTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return notifyResult{Result: notifyCategory(err)}
	}
	defer res.Body.Close()
	result := notifyResult{Status: res.StatusCode}
	if strings.HasPrefix(id, "msg_verification_") {
		result.body, err = io.ReadAll(io.LimitReader(res.Body, 4096))
		if err != nil {
			return notifyResult{Status: res.StatusCode, Result: notifyCategory(err)}
		}
	}
	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
		result.Delivered = true
		result.Result = "ok"
	case res.StatusCode >= 300 && res.StatusCode < 400:
		result.Result = "redirect"
	case res.StatusCode >= 400 && res.StatusCode < 500:
		result.Result = "http_4xx"
	default:
		result.Result = "http_5xx"
	}
	return result
}
