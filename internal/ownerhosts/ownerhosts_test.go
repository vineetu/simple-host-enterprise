package ownerhosts

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
)

type fakeKube struct {
	template  map[string]any
	applied   map[string]map[string]any
	managed   []string
	deleted   []string
	ready     map[string]bool
	applyErr  map[string]error
	certErr   map[string]error
	listErr   error
	getErr    error
	deleteErr map[string]error
}

func (k *fakeKube) GetIngress(_ context.Context, name string) (map[string]any, error) {
	if k.getErr != nil {
		return nil, k.getErr
	}
	if name != "simple-host" {
		return nil, errors.New("no such ingress " + name)
	}
	return k.template, nil
}

func (k *fakeKube) ApplyIngress(_ context.Context, name string, body map[string]any) error {
	if err := k.applyErr[name]; err != nil {
		return err
	}
	k.applied[name] = body
	return nil
}

func (k *fakeKube) ListManagedIngresses(context.Context) ([]string, error) {
	return k.managed, k.listErr
}

func (k *fakeKube) DeleteIngress(_ context.Context, name string) error {
	if err := k.deleteErr[name]; err != nil {
		return err
	}
	k.deleted = append(k.deleted, name)
	return nil
}

func (k *fakeKube) CertificateReady(_ context.Context, name string) (bool, error) {
	if err := k.certErr[name]; err != nil {
		return false, err
	}
	return k.ready[name], nil
}

type fakeStore struct {
	owners   []string
	ownerErr error
	ready    map[string]bool
	setErr   map[string]error
	kept     []string
	keptSet  bool
}

func (s *fakeStore) OwnerLabelsWithSites(context.Context) ([]string, error) {
	return s.owners, s.ownerErr
}

func (s *fakeStore) SetReady(_ context.Context, owner string, ready bool) error {
	if err := s.setErr[owner]; err != nil {
		return err
	}
	s.ready[owner] = ready
	return nil
}

func (s *fakeStore) DeleteExcept(_ context.Context, keep []string) error {
	s.kept, s.keptSet = keep, true
	return nil
}

func templateIngress() map[string]any {
	backend := map[string]any{"service": map[string]any{"name": "simple-host", "port": map[string]any{"number": float64(80)}}}
	return map[string]any{
		"metadata": map[string]any{
			"name": "simple-host",
			"annotations": map[string]any{
				"nginx.ingress.kubernetes.io/proxy-body-size":      "100m",
				"cert-manager.io/cluster-issuer":                   "template-issuer",
				"cert-manager.io/common-name":                      "sites.example.com",
				"kubectl.kubernetes.io/last-applied-configuration": "{}",
			},
		},
		"spec": map[string]any{
			"ingressClassName": "nginx",
			"rules": []any{map[string]any{
				"host": "sites.example.com",
				"http": map[string]any{"paths": []any{map[string]any{"path": "/", "pathType": "Prefix", "backend": backend}}},
			}},
		},
	}
}

func newFixture(owners ...string) (*fakeKube, *fakeStore, Reconciler) {
	kube := &fakeKube{
		template:  templateIngress(),
		applied:   map[string]map[string]any{},
		ready:     map[string]bool{},
		applyErr:  map[string]error{},
		certErr:   map[string]error{},
		deleteErr: map[string]error{},
	}
	store := &fakeStore{owners: owners, ready: map[string]bool{}, setErr: map[string]error{}}
	return kube, store, Reconciler{Kube: kube, Store: store, Base: "sites.example.com", Issuer: "letsencrypt", Template: "simple-host"}
}

func TestOnceAppliesOneIngressPerOwner(t *testing.T) {
	kube, store, r := newFixture("alice", "team-sales")
	kube.ready[SecretName("alice")] = true
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(kube.applied) != 2 {
		t.Fatalf("applied %d ingresses, want 2", len(kube.applied))
	}
	body := kube.applied["sh-owner-alice"]
	if body == nil {
		t.Fatalf("no ingress sh-owner-alice; applied %v", kube.applied)
	}
	meta := body["metadata"].(map[string]any)
	if meta["name"] != "sh-owner-alice" {
		t.Errorf("name = %v", meta["name"])
	}
	labels := meta["labels"].(map[string]any)
	if labels[managedByLabel] != managedByValue || labels[ownerLabelKey] != "alice" {
		t.Errorf("labels = %v", labels)
	}
	wantAnnotations := map[string]any{
		"nginx.ingress.kubernetes.io/proxy-body-size": "100m",
		"cert-manager.io/cluster-issuer":              "letsencrypt",
	}
	if got := meta["annotations"]; !reflect.DeepEqual(got, wantAnnotations) {
		t.Errorf("annotations = %v, want %v", got, wantAnnotations)
	}
	spec := body["spec"].(map[string]any)
	if spec["ingressClassName"] != "nginx" {
		t.Errorf("ingressClassName = %v", spec["ingressClassName"])
	}
	tls := spec["tls"].([]any)[0].(map[string]any)
	if tls["secretName"] != "sh-owner-alice-tls" || !reflect.DeepEqual(tls["hosts"], []any{"*.alice.sites.example.com"}) {
		t.Errorf("tls = %v", tls)
	}
	rule := spec["rules"].([]any)[0].(map[string]any)
	if rule["host"] != "*.alice.sites.example.com" {
		t.Errorf("rule host = %v", rule["host"])
	}
	path := rule["http"].(map[string]any)["paths"].([]any)[0].(map[string]any)
	wantBackend := templateIngress()["spec"].(map[string]any)["rules"].([]any)[0].(map[string]any)["http"].(map[string]any)["paths"].([]any)[0].(map[string]any)["backend"]
	if path["path"] != "/" || path["pathType"] != "Prefix" || !reflect.DeepEqual(path["backend"], wantBackend) {
		t.Errorf("path = %v", path)
	}

	if !reflect.DeepEqual(store.ready, map[string]bool{"alice": true, "team-sales": false}) {
		t.Errorf("ready = %v", store.ready)
	}
	if !reflect.DeepEqual(store.kept, []string{"alice", "team-sales"}) {
		t.Errorf("kept = %v", store.kept)
	}
	// The template's own annotations are untouched.
	if templateIngress()["metadata"].(map[string]any)["annotations"].(map[string]any)["cert-manager.io/cluster-issuer"] != "template-issuer" {
		t.Error("template changed")
	}
}

func TestOnceUsesDefaultBackendAndOmitsMissingClass(t *testing.T) {
	kube, _, r := newFixture("alice")
	kube.template = map[string]any{"spec": map[string]any{"defaultBackend": map[string]any{"service": map[string]any{"name": "x"}}}}
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	spec := kube.applied["sh-owner-alice"]["spec"].(map[string]any)
	if _, ok := spec["ingressClassName"]; ok {
		t.Errorf("ingressClassName set with none in the template: %v", spec["ingressClassName"])
	}
	path := spec["rules"].([]any)[0].(map[string]any)["http"].(map[string]any)["paths"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(path["backend"], map[string]any{"service": map[string]any{"name": "x"}}) {
		t.Errorf("backend = %v", path["backend"])
	}

	kube.template = map[string]any{"spec": map[string]any{}}
	if err := r.Once(context.Background()); err == nil {
		t.Fatal("Once accepted a template with no backend")
	}
}

func TestOnceSkipsInvalidLabels(t *testing.T) {
	long := strings.Repeat("a", 63)
	kube, store, r := newFixture("alice", "Bob", "-x", "x-", "a--b", "a.b", "a_b", "", strings.Repeat("a", 64), long)
	r.Base = strings.Repeat("b", 200) + ".example" // "*.<63>.<base>" is over 253
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	var names []string
	for name := range kube.applied {
		names = append(names, name)
	}
	if !reflect.DeepEqual(names, []string{"sh-owner-alice"}) {
		t.Errorf("applied %v, want only alice", names)
	}
	if !reflect.DeepEqual(store.kept, []string{"alice"}) {
		t.Errorf("kept = %v", store.kept)
	}
}

func TestOnceRemovesOwnersWithoutSites(t *testing.T) {
	kube, store, r := newFixture("alice")
	kube.managed = []string{"sh-owner-alice", "sh-owner-gone", "sh-owner-old"}
	kube.deleteErr["sh-owner-gone"] = errors.New("forbidden")
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(store.kept, []string{"alice"}) {
		t.Errorf("kept = %v", store.kept)
	}
	// One failed delete does not stop the next.
	if !reflect.DeepEqual(kube.deleted, []string{"sh-owner-old"}) {
		t.Errorf("deleted = %v", kube.deleted)
	}

	// No owners at all: every row and every managed ingress goes.
	kube, store, r = newFixture()
	kube.managed = []string{"sh-owner-alice"}
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !store.keptSet || len(store.kept) != 0 || !reflect.DeepEqual(kube.deleted, []string{"sh-owner-alice"}) {
		t.Errorf("kept %v (set %t), deleted %v", store.kept, store.keptSet, kube.deleted)
	}
}

// An error for one owner is logged and the pass goes on: every other owner
// is still applied and recorded, and nobody's ingress is deleted for it.
func TestOnceErrorsForOneOwnerDoNotStopOthers(t *testing.T) {
	kube, store, r := newFixture("a", "b", "c", "d")
	for _, o := range []string{"a", "b", "c", "d"} {
		kube.ready[SecretName(o)] = true
	}
	kube.applyErr["sh-owner-a"] = errors.New("apply failed")
	kube.certErr[SecretName("b")] = errors.New("cert read failed")
	store.setErr["c"] = errors.New("db down")
	kube.managed = []string{"sh-owner-a", "sh-owner-b", "sh-owner-c", "sh-owner-d"}
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(store.ready, map[string]bool{"d": true}) {
		t.Errorf("ready = %v, want only d recorded", store.ready)
	}
	kept := append([]string(nil), store.kept...)
	sort.Strings(kept)
	if !reflect.DeepEqual(kept, []string{"a", "b", "c", "d"}) {
		t.Errorf("kept = %v: a failing owner must keep its row", store.kept)
	}
	if len(kube.deleted) != 0 {
		t.Errorf("deleted %v", kube.deleted)
	}
}

func TestOnceFailsWhenItCannotLearnWhatToDo(t *testing.T) {
	_, store, r := newFixture("alice")
	store.ownerErr = errors.New("db down")
	if err := r.Once(context.Background()); err == nil {
		t.Error("owner list error: want an error")
	}

	kube, store, r := newFixture("alice")
	kube.getErr = errors.New("forbidden")
	if err := r.Once(context.Background()); err == nil || store.keptSet {
		t.Errorf("template error: err %v, rows touched %t", err, store.keptSet)
	}

	kube, _, r = newFixture("alice")
	kube.listErr = errors.New("forbidden")
	if err := r.Once(context.Background()); err == nil {
		t.Error("list error: want an error")
	}
}

func TestNewInClusterOutsideAPod(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	if _, err := NewInCluster(); err == nil {
		t.Fatal("NewInCluster succeeded outside a pod")
	}
}

func TestNames(t *testing.T) {
	if IngressName("alice") != "sh-owner-alice" || SecretName("alice") != "sh-owner-alice-tls" {
		t.Fatalf("names = %q, %q", IngressName("alice"), SecretName("alice"))
	}
}
