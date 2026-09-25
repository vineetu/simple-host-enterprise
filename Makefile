# Simple Host: build, test, and run the package on a local cluster.
#
#   make test          go test ./...
#   make test-db       the migration chain against a throwaway Postgres in Docker
#   make vuln          govulncheck
#   make image         docker build -t simple-host:local
#   make local         bring the whole thing up on Docker Desktop Kubernetes
#   make smoke         run scripts/smoke.sh against the local overlay
#                      (or, with BASE=https://<host> KEY_FILE=<path>, a real install)
#   make local-down    delete it, data included
#
# Every kubectl call names the context explicitly. The default is Docker
# Desktop; `make local CLUSTER_CONTEXT=minikube` targets minikube, which also
# needs `minikube addons enable ingress` replaced by the manifest below and
# `minikube tunnel` running so the LoadBalancer reaches loopback.

# docker-desktop is the evaluation default. A real install is on whatever
# context you are already pointed at, so that is what the install targets use
# unless you say otherwise — pinning a cloud's context name here would be the
# one line in this file that assumed a provider.
CLUSTER_CONTEXT ?= docker-desktop
INSTALL_CONTEXT ?= $(shell kubectl config current-context 2>/dev/null)
NAMESPACE       := simple-host
LOCAL_BASE      ?= simple-host.127-0-0-1.nip.io
LOCAL_OVERLAY   := deploy/overlays/local
KUBECTL         := kubectl --context $(CLUSTER_CONTEXT)

# Pinned third-party manifests. Bump deliberately; both carry CRDs.
INGRESS_NGINX_URL := https://raw.githubusercontent.com/kubernetes/ingress-nginx/controller-v1.14.1/deploy/static/provider/cloud/deploy.yaml
CERT_MANAGER_URL  := https://github.com/cert-manager/cert-manager/releases/download/v1.19.2/cert-manager.yaml

.PHONY: build test test-db vuln image local local-tools local-third-party local-certs local-secrets local-image-load local-up local-down smoke pentest render

# Stamped into the binary; `simple-host version` and the first log line show it.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)" -o bin/simple-host ./cmd/server

test:
	go test ./...

# Runs the embedded migration chain, start to finish, on a fresh database.
test-db:
	docker rm -f simple-host-test-pg >/dev/null 2>&1 || true
	docker run -d --name simple-host-test-pg -p 55432:5432 \
	  -e POSTGRES_USER=test -e POSTGRES_PASSWORD=test -e POSTGRES_DB=test postgres:16.11 >/dev/null
	@until docker exec simple-host-test-pg pg_isready -U test >/dev/null 2>&1; do sleep 1; done
	MIGRATE_TEST_DSN="postgres://test:test@localhost:55432/test?sslmode=disable" go test ./internal/migrate/ -run TestApplyAgainstPostgres -v
	docker rm -f simple-host-test-pg >/dev/null

vuln:
	govulncheck ./...

image:
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t simple-host:local .

render:
	kustomize build $(LOCAL_OVERLAY) >/dev/null && echo "local overlay renders"

local: local-tools local-third-party local-certs local-secrets image local-image-load local-up
	@echo
	@echo "Simple Host is up at https://$(LOCAL_BASE)"
	@echo "Sign in at https://$(LOCAL_BASE)/auth/login as admin@example.com / adminpass"
	@echo "or person@example.com / personpass (Dex's two static test accounts;"
	@echo "see deploy/components/dex/configmap.yaml), then mint a key from /dashboard."

local-tools:
	@for tool in docker kubectl kustomize mkcert; do \
	  command -v $$tool >/dev/null || { echo "missing: $$tool"; exit 1; }; done
	@$(KUBECTL) cluster-info >/dev/null 2>&1 || { echo "kubectl context $(CLUSTER_CONTEXT) is not reachable"; exit 1; }

# ingress-nginx and cert-manager are applied ahead of the overlay because the
# overlay contains Certificate objects, and those need the CRDs established
# and the webhook answering first.
local-third-party:
	$(KUBECTL) apply -f $(INGRESS_NGINX_URL)
	$(KUBECTL) apply -f $(CERT_MANAGER_URL)
	$(KUBECTL) -n ingress-nginx rollout status deploy/ingress-nginx-controller --timeout=300s
	$(KUBECTL) -n cert-manager rollout status deploy/cert-manager --timeout=300s
	$(KUBECTL) -n cert-manager rollout status deploy/cert-manager-webhook --timeout=300s
	$(KUBECTL) -n cert-manager rollout status deploy/cert-manager-cainjector --timeout=300s

# mkcert installs a local CA into the system and browser trust stores, then
# issues the base host and its wildcard. __Host- cookies need HTTPS, and a
# certificate the browser trusts is the only way to test them honestly.
local-certs:
	mkcert -install
	mkcert -cert-file $(LOCAL_OVERLAY)/tls.crt -key-file $(LOCAL_OVERLAY)/tls.key "$(LOCAL_BASE)" "*.$(LOCAL_BASE)"
	$(KUBECTL) create namespace $(NAMESPACE) --dry-run=client -o yaml | $(KUBECTL) apply -f -
	$(KUBECTL) -n $(NAMESPACE) create secret tls simple-host-local-tls \
	  --cert=$(LOCAL_OVERLAY)/tls.crt --key=$(LOCAL_OVERLAY)/tls.key --dry-run=client -o yaml | $(KUBECTL) apply -f -

# secrets.env is generated once with random values and never committed.
# DB_PASSWORD is the owning role migrations run as; DB_APP_PASSWORD is the
# distinct least-privilege application role the server connects as (design
# 9.3) — the two must never be the same value.
local-secrets:
	@test -f $(LOCAL_OVERLAY)/secrets.env || { \
	  printf 'DB_PASSWORD=%s\nDB_APP_PASSWORD=%s\nBACKUP_STORAGE_ACCESS_KEY_ID=simplehost\nBACKUP_STORAGE_SECRET_ACCESS_KEY=%s\nMINIO_KMS_SECRET_KEY=local-sse-key:%s\nOIDC_CLIENT_SECRET=local-dex-client-secret-96808aacbcb9ec41\nSESSION_SIGNING_KEY=local1:%s\n' \
	    "$$(openssl rand -hex 24)" "$$(openssl rand -hex 24)" "$$(openssl rand -hex 24)" "$$(openssl rand -base64 32)" "$$(openssl rand -base64 32)" > $(LOCAL_OVERLAY)/secrets.env; \
	  echo "wrote $(LOCAL_OVERLAY)/secrets.env"; \
	  echo "NOTE: OIDC_CLIENT_SECRET must match deploy/components/dex/configmap.yaml's staticClients[0].secret exactly — it is fixed, not random, above."; }

# Docker Desktop's node has its own image store; the build must be copied in.
local-image-load:
	scripts/load-image.sh simple-host:local $(CLUSTER_CONTEXT)

local-up:
	kustomize build $(LOCAL_OVERLAY) | $(KUBECTL) apply -f -
	$(KUBECTL) -n $(NAMESPACE) rollout status statefulset/postgres --timeout=300s
	$(KUBECTL) -n $(NAMESPACE) rollout status statefulset/minio --timeout=300s
	$(KUBECTL) -n $(NAMESPACE) rollout status deploy/dex --timeout=180s
	$(KUBECTL) -n $(NAMESPACE) rollout status deploy/simple-host --timeout=300s

local-down:
	kustomize build $(LOCAL_OVERLAY) | $(KUBECTL) delete --ignore-not-found -f -
	$(KUBECTL) -n $(NAMESPACE) delete pvc --all --ignore-not-found

# CURL_CA_BUNDLE points curl at the mkcert root so the run does not depend on
# `mkcert -install` having been accepted into the system trust store.
# CLUSTER_CONTEXT is passed through: smoke.sh port-forwards Dex's Service to
# reach its login form, since Dex's issuer is the in-cluster DNS name (see
# deploy/components/dex/configmap.yaml) and only the simple-host pod, not
# this script, can resolve that name directly.
#
# With BASE set, smoke targets a real install instead, over public HTTPS
# only (scripts/smoke-remote.sh): no kubectl, no database, no Dex. It needs
# an API key an admin minted on /dashboard, read from KEY_FILE, else from
# SIMPLE_HOST_API_KEY, else from a hidden prompt; it is never printed.
#   make smoke BASE=https://sites.example.com KEY_FILE=~/.simple-host-install-key
# OTHER_KEY_FILE (optional) is a second, non-admin person's key; with it the
# run also proves a signed-in stranger gets 404 from a restricted site.
BASE           ?=
KEY_FILE       ?=
OTHER_KEY_FILE ?=
smoke:
	@if [ -n "$(BASE)" ]; then \
	  BASE="$(BASE)" KEY_FILE="$(KEY_FILE)" OTHER_KEY_FILE="$(OTHER_KEY_FILE)" ./scripts/smoke-remote.sh; \
	else \
	  CURL_CA_BUNDLE="$$(mkcert -CAROOT)/rootCA.pem" BASE=$(LOCAL_BASE) \
	    CLUSTER_CONTEXT="$(CLUSTER_CONTEXT)" NAMESPACE="$(NAMESPACE)" ./scripts/smoke.sh; \
	fi

# Phase 7's scripted pen-test list (design.md 12) against the local overlay.
# Dex's issuer is the in-cluster Service DNS name (see
# deploy/components/dex/configmap.yaml), so this starts the same port-forward
# smoke does. `/auth/*` is rate-limited (Burst 20, refill 0.2/s -
# docs/install.md section 10); running the whole package with plain `-v` can
# exhaust it, so this paces sign-in-heavy tests apart rather than firing all
# of them at once - see docs/security-review.md section 3 for the
# per-test timing this was tuned against. Set PENTEST_SESSION_IDLE (matching
# the target's own SESSION_IDLE) to also exercise TestIdleSessionSurvival;
# it is skipped without it. -timeout 30m matters once that variable is set.
pentest:
	$(KUBECTL) -n $(NAMESPACE) port-forward svc/dex 5556:5556 >/tmp/pentest-dex-pf.log 2>&1 & \
	  pf=$$!; trap 'kill $$pf' EXIT; sleep 2; \
	  PENTEST_BASE=https://$(LOCAL_BASE) \
	  PENTEST_OWNER_HOST_SUFFIX=$(LOCAL_BASE) \
	  PENTEST_CA_BUNDLE="$$(mkcert -CAROOT)/rootCA.pem" \
	  PENTEST_SESSION_IDLE="$(PENTEST_SESSION_IDLE)" \
	  go test -tags pentest ./test/pentest/ -v -timeout 30m

# ---------------------------------------------------------------------------
# Installing onto a real cluster
#
# `make local` runs an evaluation cluster end to end. These do the same for a
# cluster you already have, on any provider: nothing below names a cloud.
# OVERLAY is whichever directory under deploy/overlays you have filled in.
# ---------------------------------------------------------------------------

OVERLAY   ?= deploy/overlays/byo
KUSTOMIZE ?= kustomize
ALLOW_INCLUSTER_POSTGRES ?=
# The namespace the chosen overlay installs into (staging: simple-host-stage).
INSTALL_NAMESPACE = $(shell sed -n 's/^namespace: *//p' "$(OVERLAY)/kustomization.yaml" 2>/dev/null | head -1)
# Pass --context only when there is one; an empty flag is a hard error.
INSTALL_KUBECTL := kubectl $(if $(INSTALL_CONTEXT),--context $(INSTALL_CONTEXT))

# prereqs installs what the manifests assume the cluster already has, and
# skips whatever is present. Both are ordinary upstream projects that run on
# any conformant cluster; `make local` installs them too, which is exactly why
# they were easy to forget on a real one — the base manifests reference
# cert-manager CRDs and fail to apply without them, with an error that names
# the CRD rather than the cause.
#
# INGRESS decides the ingress controller:
#   auto  (default) install ingress-nginx only when the cluster has no
#         IngressClass at all. Any existing controller (ALB, GKE, Traefik,
#         AGIC, an ingress-nginx under another name) is used as it is: a
#         second controller would race the first for the same Ingress.
#   nginx install ingress-nginx unless a class called nginx already exists,
#         even alongside another controller.
#   none  never touch the ingress controller.
# A freshly installed ingress-nginx is marked the default IngressClass when it
# is the only one, so an overlay that names no class still lands on it.
INGRESS ?= auto
.PHONY: prereqs
prereqs:
	@case "$(INGRESS)" in auto|nginx|none) ;; \
	  *) echo "FAIL  INGRESS must be auto, nginx or none (got $(INGRESS))"; exit 1;; esac; \
	classes=$$($(INSTALL_KUBECTL) get ingressclass -o name) || { \
	  echo "FAIL  cannot list IngressClasses on this context; fix access before installing anything"; exit 1; }; \
	classes=$$(printf '%s\n' "$$classes" | sed 's|.*/||' | tr '\n' ' ' | sed 's/ *$$//'); \
	install=0; \
	case "$(INGRESS)" in \
	  none)  echo "==> ingress: INGRESS=none, leaving the controller alone (classes: $${classes:-none})";; \
	  nginx) if $(INSTALL_KUBECTL) get ingressclass nginx >/dev/null 2>&1; then \
	           echo "==> ingress-nginx: already installed"; else install=1; fi;; \
	  auto)  if [ -n "$$classes" ]; then \
	           echo "==> ingress: using the cluster's existing IngressClass ($$classes); not installing ingress-nginx"; \
	         else install=1; fi;; \
	esac; \
	if [ $$install -eq 1 ]; then \
	  echo "==> installing ingress-nginx"; \
	  $(INSTALL_KUBECTL) apply -f $(INGRESS_NGINX_URL) || exit 1; \
	  $(INSTALL_KUBECTL) -n ingress-nginx rollout status deploy/ingress-nginx-controller --timeout=300s || exit 1; \
	  if [ -z "$$classes" ]; then \
	    $(INSTALL_KUBECTL) annotate ingressclass nginx ingressclass.kubernetes.io/is-default-class=true --overwrite || exit 1; fi; \
	fi
	@if $(INSTALL_KUBECTL) get crd certificates.cert-manager.io >/dev/null 2>&1; then \
	  echo "==> cert-manager: already installed"; \
	else \
	  echo "==> installing cert-manager"; \
	  $(INSTALL_KUBECTL) apply -f $(CERT_MANAGER_URL); \
	  $(INSTALL_KUBECTL) -n cert-manager rollout status deploy/cert-manager-webhook --timeout=300s; \
	  $(INSTALL_KUBECTL) -n cert-manager rollout status deploy/cert-manager-cainjector --timeout=300s; \
	fi

# preflight fails loudly, before anything is applied, on the mistakes that
# otherwise surface as a healthy-looking instance doing something wrong.
.PHONY: preflight
preflight:
	@fail=0; \
	if [ ! -f "$(OVERLAY)/kustomization.yaml" ]; then \
	  echo "FAIL  $(OVERLAY) has no kustomization.yaml — set OVERLAY=<dir>"; exit 1; fi; \
	if grep -q "REPLACE_WITH" "$(OVERLAY)/kustomization.yaml" 2>/dev/null; then \
	  echo "FAIL  $(OVERLAY)/kustomization.yaml still has a REPLACE_WITH placeholder."; \
	  echo "      Published images and their digests are on the repository's releases page."; fail=1; fi; \
	if $(KUSTOMIZE) build "$(OVERLAY)" 2>/dev/null | grep -q "image: .*REPLACE_WITH"; then \
	  echo "FAIL  the rendered image is still the base's placeholder digest; set images: in $(OVERLAY)/kustomization.yaml"; fail=1; fi; \
	if $(KUSTOMIZE) build "$(OVERLAY)" 2>/dev/null | grep -q "DB_INCLUSTER_EVALUATION"; then \
	  if [ "$(OVERLAY)" = "$(LOCAL_OVERLAY)" ] || [ "$(ALLOW_INCLUSTER_POSTGRES)" = "1" ]; then \
	    echo "WARN  in-cluster evaluation Postgres: nothing backs it up. Use a managed Postgres with point-in-time recovery for anything real."; \
	  else \
	    echo "FAIL  $(OVERLAY) includes components/postgres-incluster, which nothing backs up."; \
	    echo "      Use a managed Postgres with point-in-time recovery, or set ALLOW_INCLUSTER_POSTGRES=1 for a throwaway install."; fail=1; fi; fi; \
	for f in config.env ingress-patch.yaml; do \
	  [ -f "$(OVERLAY)/$$f" ] || continue; \
	  hits=$$(grep -nE '^[^#]*(REPLACE_WITH|REPLACE_ME|example\.com|example-idp)' "$(OVERLAY)/$$f" | cut -d: -f1 | tr '\n' ' '); \
	  if [ -n "$$hits" ]; then \
	    echo "FAIL  $(OVERLAY)/$$f still has example placeholders on line(s) $$hits"; fail=1; fi; \
	done; \
	for f in config.env secrets.env; do \
	  if [ -f "$(OVERLAY)/$$f.example" ] && [ ! -f "$(OVERLAY)/$$f" ]; then \
	    echo "FAIL  $(OVERLAY)/$$f is missing — copy $$f.example and fill it in"; fail=1; fi; \
	done; \
	if $(KUSTOMIZE) build "$(OVERLAY)" 2>/dev/null | grep -q "imagePullSecrets"; then :; else \
	  img=$$($(KUSTOMIZE) build "$(OVERLAY)" 2>/dev/null | grep -m1 "image: " | sed 's/.*image: //'); \
	  case "$$img" in \
	    ghcr.io/*|"" ) : ;; \
	    *) echo "WARN  $$img looks like a private registry and no imagePullSecrets is set."; \
	       echo "      Put it on the ServiceAccount, not the Deployment: three workloads pull"; \
	       echo "      this image, and patching only the Deployment leaves both CronJobs in"; \
	       echo "      ImagePullBackOff — backups and audit pruning silently never run.";; \
	  esac; fi; \
	class=$$($(KUSTOMIZE) build "$(OVERLAY)" 2>/dev/null | sed -n 's/^ *ingressClassName: *//p' | tr -d '"' | head -1); \
	annotated=$$($(KUSTOMIZE) build "$(OVERLAY)" 2>/dev/null | sed -n 's/^ *kubernetes.io\/ingress.class: *//p' | tr -d '"' | head -1); \
	if avail=$$($(INSTALL_KUBECTL) get ingressclass -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.annotations.ingressclass\.kubernetes\.io/is-default-class}{" "}{end}' 2>/dev/null); then \
	  names=$$(printf '%s' "$$avail" | sed 's/=[^ ]*//g; s/ *$$//'); \
	  default=$$(printf '%s' "$$avail" | tr ' ' '\n' | sed -n 's/=true$$//p' | head -1); \
	  if [ -z "$$class" ] && [ -n "$$annotated" ]; then \
	    echo "==> ingress class: $$annotated (kubernetes.io/ingress.class annotation, e.g. GKE)"; \
	  elif [ -z "$$names" ]; then \
	    echo "NOTE  the cluster has no IngressClass yet; make install adds ingress-nginx (INGRESS=auto)"; \
	  elif [ -n "$$class" ]; then \
	    case " $$names " in *" $$class "*) echo "==> ingress class: $$class";; \
	      *) echo "FAIL  the overlay's ingressClassName is $$class, but the cluster has: $$names"; \
	         echo "      Set it in $(OVERLAY)/ingress-patch.yaml, or delete the line to use the default class."; fail=1;; esac; \
	  elif [ -n "$$default" ]; then \
	    echo "==> ingress class: the cluster default, $$default"; \
	  else \
	    echo "FAIL  the overlay names no ingressClassName and the cluster has no default IngressClass."; \
	    echo "      Set ingressClassName in $(OVERLAY)/ingress-patch.yaml to one of: $$names"; fail=1; \
	  fi; \
	else \
	  echo "WARN  cluster not reachable; the ingress class was not checked"; \
	fi; \
	[ $$fail -eq 0 ] || exit 1; \
	echo "==> preflight ok"

# install is the whole thing: prerequisites, the checks, then apply and wait.
.PHONY: install
install: prereqs preflight
	$(KUSTOMIZE) build "$(OVERLAY)" | $(INSTALL_KUBECTL) apply -f -
	$(INSTALL_KUBECTL) -n $(or $(INSTALL_NAMESPACE),$(NAMESPACE)) rollout status deploy/simple-host --timeout=300s
	@echo "==> installed. Next (INSTALL.md sections 7-9): wait until 'kubectl -n $(or $(INSTALL_NAMESPACE),$(NAMESPACE)) get certificate' shows READY True"
	@echo "    (or your cloud's managed certificate is active) and https://<your base host>/readyz answers;"
	@echo "    then an admin signs in, mints an API key on /dashboard, and you verify with:"
	@echo "    make smoke BASE=https://<your base host> KEY_FILE=<file holding the key>"
