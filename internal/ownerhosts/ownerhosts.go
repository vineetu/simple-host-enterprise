// Package ownerhosts keeps one Ingress per owner, "*.<owner>.<base>", so
// cert-manager issues that owner's wildcard certificate, and records in
// owner_hosts which owners' certificates are ready. A TLS wildcard covers
// one label, so "<site>.<owner>.<base>" needs a certificate per owner; the
// server hands out and redirects to those hosts only for owners recorded
// ready here (handler.OwnerHostReadiness).
//
// It runs as its own Deployment (`simple-host owner-hosts`) under its own
// ServiceAccount, whose Role allows ingresses and reading cert-manager
// Certificates in this namespace and nothing else, so the server's own pods
// keep no Kubernetes credential at all. It talks to the API server over
// plain HTTPS with the mounted token; no client library.
package ownerhosts

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/db"
)

const (
	saDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	// managedByLabel marks the Ingresses this reconciler owns, so cleanup
	// never touches anything else in the namespace.
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "simple-host-owner-hosts"
	ownerLabelKey  = "simple-host/owner"
	fieldManager   = "simple-host-owner-hosts"
)

// ownerLabelRe is the owner label rule (handler.isValidLabel, no "--"):
// anything else is never spliced into a host or an object name.
var ownerLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func validOwnerLabel(l string) bool {
	return ownerLabelRe.MatchString(l) && !strings.Contains(l, "--")
}

// IngressName and SecretName name an owner's Ingress and its TLS Secret
// (and so the Certificate cert-manager's ingress-shim creates for it).
func IngressName(owner string) string { return "sh-owner-" + owner }
func SecretName(owner string) string  { return "sh-owner-" + owner + "-tls" }

// Kube is the part of the Kubernetes API the reconciler uses.
type Kube interface {
	GetIngress(ctx context.Context, name string) (map[string]any, error)
	ApplyIngress(ctx context.Context, name string, body map[string]any) error
	ListManagedIngresses(ctx context.Context) ([]string, error)
	DeleteIngress(ctx context.Context, name string) error
	CertificateReady(ctx context.Context, name string) (bool, error)
}

// Store is the part of the database the reconciler uses.
type Store interface {
	OwnerLabelsWithSites(ctx context.Context) ([]string, error)
	SetReady(ctx context.Context, owner string, ready bool) error
	DeleteExcept(ctx context.Context, keep []string) error
}

// Reconciler brings the Ingresses and owner_hosts in line with the owners
// that have sites.
type Reconciler struct {
	Kube     Kube
	Store    Store
	Base     string // base host, e.g. "sites.example.com"
	Issuer   string // cert-manager ClusterIssuer
	Template string // the install's own Ingress
}

// Once runs one pass. An error for one owner is logged and the pass goes
// on; the pass fails only when it cannot learn what to do at all.
func (r Reconciler) Once(ctx context.Context) error {
	owners, err := r.Store.OwnerLabelsWithSites(ctx)
	if err != nil {
		return fmt.Errorf("list owners: %w", err)
	}
	template, err := r.Kube.GetIngress(ctx, r.Template)
	if err != nil {
		return fmt.Errorf("read template ingress %q: %w", r.Template, err)
	}
	want := map[string]bool{}
	keep := []string{}
	for _, owner := range owners {
		if !validOwnerLabel(owner) || len("*."+owner+"."+r.Base) > 253 {
			continue
		}
		want[IngressName(owner)] = true
		keep = append(keep, owner)
		body, err := ownerIngress(template, owner, r.Base, r.Issuer)
		if err != nil {
			return err
		}
		if err := r.Kube.ApplyIngress(ctx, IngressName(owner), body); err != nil {
			log.Printf("owner hosts: apply ingress for %s: %v", owner, err)
			continue
		}
		ready, err := r.Kube.CertificateReady(ctx, SecretName(owner))
		if err != nil {
			log.Printf("owner hosts: certificate for %s: %v", owner, err)
			continue
		}
		if err := r.Store.SetReady(ctx, owner, ready); err != nil {
			log.Printf("owner hosts: record %s ready=%t: %v", owner, ready, err)
		}
	}
	// An owner with no sites left needs no certificate. The row goes first,
	// so the server stops sending anyone to that host before it loses its
	// certificate.
	if err := r.Store.DeleteExcept(ctx, keep); err != nil {
		return fmt.Errorf("forget owners without sites: %w", err)
	}
	managed, err := r.Kube.ListManagedIngresses(ctx)
	if err != nil {
		return fmt.Errorf("list owner ingresses: %w", err)
	}
	for _, name := range managed {
		if want[name] {
			continue
		}
		if err := r.Kube.DeleteIngress(ctx, name); err != nil {
			log.Printf("owner hosts: delete ingress %s: %v", name, err)
		}
	}
	return nil
}

// Run passes every interval until ctx ends.
func (r Reconciler) Run(ctx context.Context, interval time.Duration) {
	for {
		if err := r.Once(ctx); err != nil {
			log.Printf("owner hosts: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// ownerIngress builds the owner's Ingress from the install's own: the same
// class, the same controller annotations (body size and the like), the same
// backend, and one wildcard host whose TLS secret cert-manager's
// ingress-shim fills from issuer.
func ownerIngress(template map[string]any, owner, base, issuer string) (map[string]any, error) {
	spec, _ := template["spec"].(map[string]any)
	backend, err := templateBackend(spec)
	if err != nil {
		return nil, err
	}
	annotations := map[string]any{}
	if meta, ok := template["metadata"].(map[string]any); ok {
		if a, ok := meta["annotations"].(map[string]any); ok {
			for k, v := range a {
				if strings.HasPrefix(k, "cert-manager.io/") || strings.HasPrefix(k, "kubectl.kubernetes.io/") {
					continue
				}
				annotations[k] = v
			}
		}
	}
	annotations["cert-manager.io/cluster-issuer"] = issuer
	host := "*." + owner + "." + base
	outSpec := map[string]any{
		"tls": []any{map[string]any{"hosts": []any{host}, "secretName": SecretName(owner)}},
		"rules": []any{map[string]any{
			"host": host,
			"http": map[string]any{"paths": []any{map[string]any{
				"path": "/", "pathType": "Prefix", "backend": backend,
			}}},
		}},
	}
	if class, ok := spec["ingressClassName"].(string); ok && class != "" {
		outSpec["ingressClassName"] = class
	}
	return map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "Ingress",
		"metadata": map[string]any{
			"name":        IngressName(owner),
			"labels":      map[string]any{managedByLabel: managedByValue, ownerLabelKey: owner},
			"annotations": annotations,
		},
		"spec": outSpec,
	}, nil
}

func templateBackend(spec map[string]any) (any, error) {
	rules, _ := spec["rules"].([]any)
	for _, rule := range rules {
		r, _ := rule.(map[string]any)
		h, _ := r["http"].(map[string]any)
		paths, _ := h["paths"].([]any)
		for _, p := range paths {
			pm, _ := p.(map[string]any)
			if b, ok := pm["backend"]; ok {
				return b, nil
			}
		}
	}
	if b, ok := spec["defaultBackend"]; ok {
		return b, nil
	}
	return nil, errors.New("template ingress has no backend to copy")
}

// DBStore is Store over the server's database.
type DBStore struct{ DB *sql.DB }

func (s DBStore) OwnerLabelsWithSites(ctx context.Context) ([]string, error) {
	return db.OwnerLabelsWithSites(ctx, s.DB)
}
func (s DBStore) SetReady(ctx context.Context, owner string, ready bool) error {
	return db.SetOwnerHostReady(ctx, s.DB, owner, ready)
}
func (s DBStore) DeleteExcept(ctx context.Context, keep []string) error {
	return db.DeleteOwnerHostsExcept(ctx, s.DB, keep)
}

// InCluster is Kube over the API server this pod runs in, authenticated by
// its mounted ServiceAccount token (re-read on every call, since it
// rotates).
type InCluster struct {
	server    string
	namespace string
	client    *http.Client
}

// NewInCluster reads the pod's ServiceAccount mount.
func NewInCluster() (*InCluster, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not running in a Kubernetes pod (KUBERNETES_SERVICE_HOST unset)")
	}
	ns, err := os.ReadFile(saDir + "/namespace")
	if err != nil {
		return nil, fmt.Errorf("read namespace: %w", err)
	}
	caPEM, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("read cluster CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("cluster CA bundle has no certificate")
	}
	return &InCluster{
		server:    "https://" + net.JoinHostPort(host, port),
		namespace: strings.TrimSpace(string(ns)),
		client: &http.Client{
			Timeout:   20 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		},
	}, nil
}

var errNotFound = errors.New("not found")

func (k *InCluster) do(ctx context.Context, method, path, contentType string, body []byte, out any) error {
	token, err := os.ReadFile(saDir + "/token")
	if err != nil {
		return fmt.Errorf("read token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.server+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == http.StatusNotFound {
		return errNotFound
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (k *InCluster) ingressPath(name string) string {
	return "/apis/networking.k8s.io/v1/namespaces/" + url.PathEscape(k.namespace) + "/ingresses/" + url.PathEscape(name)
}

func (k *InCluster) GetIngress(ctx context.Context, name string) (map[string]any, error) {
	var out map[string]any
	err := k.do(ctx, http.MethodGet, k.ingressPath(name), "", nil, &out)
	return out, err
}

// ApplyIngress is a server-side apply: create or update in one idempotent
// call, owning only the fields it sets.
func (k *InCluster) ApplyIngress(ctx context.Context, name string, body map[string]any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	q := url.Values{"fieldManager": {fieldManager}, "force": {"true"}}
	return k.do(ctx, http.MethodPatch, k.ingressPath(name)+"?"+q.Encode(), "application/apply-patch+yaml", data, nil)
}

func (k *InCluster) ListManagedIngresses(ctx context.Context) ([]string, error) {
	var out struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	q := url.Values{"labelSelector": {managedByLabel + "=" + managedByValue}}
	path := "/apis/networking.k8s.io/v1/namespaces/" + url.PathEscape(k.namespace) + "/ingresses?" + q.Encode()
	if err := k.do(ctx, http.MethodGet, path, "", nil, &out); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out.Items))
	for _, item := range out.Items {
		names = append(names, item.Metadata.Name)
	}
	sort.Strings(names)
	return names, nil
}

func (k *InCluster) DeleteIngress(ctx context.Context, name string) error {
	err := k.do(ctx, http.MethodDelete, k.ingressPath(name), "", nil, nil)
	if errors.Is(err, errNotFound) {
		return nil
	}
	return err
}

// CertificateReady reads the Certificate ingress-shim made for an owner's
// TLS secret and reports its Ready condition. Missing is not ready.
func (k *InCluster) CertificateReady(ctx context.Context, name string) (bool, error) {
	var out struct {
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	}
	path := "/apis/cert-manager.io/v1/namespaces/" + url.PathEscape(k.namespace) + "/certificates/" + url.PathEscape(name)
	err := k.do(ctx, http.MethodGet, path, "", nil, &out)
	if errors.Is(err, errNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, c := range out.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True", nil
		}
	}
	return false, nil
}
