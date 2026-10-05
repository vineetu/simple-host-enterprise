#!/usr/bin/env bash
# Lint and render deploy/helm/simple-host-enterprise, then assert the emitted
# Kubernetes resources (not the template source). Covers unconfigured
# defaults, external and in-cluster Postgres, existing vs created issuers,
# ownerCerts=manual without issuer, http storage with
# extraConfig.BACKUP_STORAGE_INSECURE_ALLOWED, serviceAccount.annotations /
# podAnnotations / podLabels, secrets.existingSecret, extraConfig/extraSecrets,
# scheduling, and the DigitalOcean preset. Needs helm and python3.
# Usage: bash scripts/test-chart.sh
set -euo pipefail
here=$(cd "$(dirname "$0")/.." && pwd)
exec python3 - "$here" <<'PY'
import os, subprocess, sys, tempfile
from collections import defaultdict

try:
    import yaml
except ImportError:
    sys.exit("FAIL  python3-yaml is required")

here = sys.argv[1]
chart = os.path.join(here, "deploy/helm/simple-host-enterprise")
fail = 0
LAST_RAW = ""


def helm(args, check=True):
    r = subprocess.run(
        ["helm", *args],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    if check and r.returncode != 0:
        sys.stderr.write(r.stdout + r.stderr)
        raise SystemExit(f"FAIL  helm {' '.join(args)} exited {r.returncode}")
    return r


def ok(name):
    print(f"ok    {name}")


def bad(name, detail=""):
    global fail
    fail = 1
    print(f"FAIL  {name}" + (f": {detail}" if detail else ""))


r = helm(["lint", chart])
# INFO (missing icon) is fine; warnings/errors are not.
noisy = [
    ln
    for ln in (r.stdout + r.stderr).splitlines()
    if ln.strip()
    and not ln.startswith("==> Linting ")
    and not ln.startswith("[INFO] ")
    and "chart(s) linted" not in ln
]
if r.returncode != 0 or noisy:
    bad("helm lint", "\n".join(noisy) or r.stderr)
else:
    ok("helm lint")


def render(name, extra):
    args = ["template", "t", chart, "-n", "simple-host", *extra]
    r = helm(args, check=False)
    global LAST_RAW
    if r.returncode != 0:
        bad(f"render {name}", r.stderr.strip() or f"exit {r.returncode}")
        LAST_RAW = ""
        return None
    LAST_RAW = r.stdout
    path = os.path.join(tempfile.gettempdir(), f"sh-chart-{name}.yaml")
    with open(path, "w") as f:
        f.write(r.stdout)
    return path


def load(path):
    docs = [d for d in yaml.safe_load_all(open(path)) if d]
    by = defaultdict(list)
    for d in docs:
        by[(d.get("kind"), (d.get("metadata") or {}).get("name"))].append(d)
    return docs, by


def one(by, kind, name):
    xs = by[(kind, name)]
    if len(xs) != 1:
        raise AssertionError(f"wanted 1 {kind}/{name}, got {len(xs)}")
    return xs[0]


def absent(by, kind, name):
    if by[(kind, name)]:
        raise AssertionError(f"did not want {kind}/{name}")


def kinds(docs):
    return {d.get("kind") for d in docs}


def cm(by):
    return one(by, "ConfigMap", "simple-host-config")["data"] or {}


def expect(name, path, fn):
    if path is None:
        return
    try:
        fn(*load(path))
        ok(name)
    except AssertionError as e:
        bad(name, str(e))


def refuse(name, pattern, extra):
    r = helm(["template", "t", chart, "-n", "simple-host", *extra], check=False)
    err = r.stdout + r.stderr
    if r.returncode == 0:
        bad(name, "helm succeeded, wanted failure")
        return
    if pattern not in err:
        bad(name, f"wanted {pattern!r}, got:\n{err.strip()}")
        return
    ok(f"refuse {name}")


configured = ["-f", os.path.join(chart, "ci/configured.yaml")]
do_preset = [
    "-f",
    os.path.join(chart, "values-digitalocean.yaml"),
    "-f",
    os.path.join(chart, "ci/digitalocean-values.yaml"),
]


def unconfigured(docs, by):
    data = cm(by)
    one(by, "StatefulSet", "postgres")
    absent(by, "Deployment", "simple-host")
    absent(by, "Ingress", "simple-host")
    if "ClusterIssuer" in kinds(docs):
        raise AssertionError("unconfigured render created a ClusterIssuer")
    if data.get("DB_PORT") != "5432":
        raise AssertionError(f"DB_PORT {data.get('DB_PORT')}")
    if data.get("BACKUP_SSE") != "AES256":
        raise AssertionError(f"BACKUP_SSE {data.get('BACKUP_SSE')}")
    if data.get("DB_INCLUSTER_EVALUATION") != "true":
        raise AssertionError("missing DB_INCLUSTER_EVALUATION")
    raw = LAST_RAW or ""
    for s in ("digitaloceanspaces", "digitalocean:", "25060", "simple-host-letsencrypt"):
        if s in raw:
            raise AssertionError(f"default render still contains {s}")


expect(
    "unconfigured: postgres StatefulSet, no app, no ClusterIssuer, port 5432, AES256",
    render("unconfigured", []),
    unconfigured,
)


def external(docs, by):
    data = cm(by)
    if data.get("DB_HOST") != "postgres.example.com":
        raise AssertionError(f"DB_HOST {data.get('DB_HOST')}")
    if data.get("DB_PORT") != "5432":
        raise AssertionError(f"DB_PORT {data.get('DB_PORT')}")
    if "DB_INCLUSTER_EVALUATION" in data:
        raise AssertionError("external mode still set DB_INCLUSTER_EVALUATION")
    if data.get("BACKUP_STORAGE_ENDPOINT") != "https://s3.example.com":
        raise AssertionError(f"endpoint {data.get('BACKUP_STORAGE_ENDPOINT')}")
    if data.get("BACKUP_STORAGE_REGION") != "eu-west-1":
        raise AssertionError(f"region {data.get('BACKUP_STORAGE_REGION')}")
    if data.get("BACKUP_SSE") != "aws:kms":
        raise AssertionError(f"sse {data.get('BACKUP_SSE')}")
    if data.get("BACKUP_SSE_KEY_ID") != "arn:aws:kms:eu-west-1:1:key/abc":
        raise AssertionError(f"sseKeyId {data.get('BACKUP_SSE_KEY_ID')}")
    if data.get("MAX_ARCHIVE_BYTES") != "209715200":
        raise AssertionError("extraConfig MAX_ARCHIVE_BYTES missing from ConfigMap")
    if data.get("OIDC_EMAIL_CLAIM") != "preferred_username":
        raise AssertionError("extraConfig OIDC_EMAIL_CLAIM missing from ConfigMap")
    if data.get("OWNER_CERT_ISSUER") != "company-ca":
        raise AssertionError(f"OWNER_CERT_ISSUER {data.get('OWNER_CERT_ISSUER')}")
    if data.get("TRUSTED_PROXY_CIDRS") != "10.0.0.0/8":
        raise AssertionError(f"TRUSTED_PROXY_CIDRS {data.get('TRUSTED_PROXY_CIDRS')}")
    if data.get("OIDC_ISSUER") != "https://login.example.com":
        raise AssertionError(f"OIDC_ISSUER {data.get('OIDC_ISSUER')}")
    if data.get("PUBLIC_BASE_URL") != "https://sites.example.com":
        raise AssertionError(f"PUBLIC_BASE_URL {data.get('PUBLIC_BASE_URL')}")
    absent(by, "StatefulSet", "postgres")
    one(by, "Secret", "simple-host-db-ca")
    sec = one(by, "Secret", "simple-host-secrets")
    if (sec.get("stringData") or {}).get("SMTP_URL") != "smtp://mail.example.com:587":
        raise AssertionError("extraSecrets SMTP_URL missing from generated Secret")
    if "ClusterIssuer" in kinds(docs):
        raise AssertionError("existing-issuer path created a ClusterIssuer")
    ing = one(by, "Ingress", "simple-host")
    if (ing.get("spec") or {}).get("ingressClassName") != "nginx":
        raise AssertionError(f"ingressClassName {ing.get('spec', {}).get('ingressClassName')}")
    if (ing.get("metadata") or {}).get("annotations", {}).get("cert-manager.io/cluster-issuer") != "company-ca":
        raise AssertionError("Ingress is not annotated with certificates.issuer")
    dep = one(by, "Deployment", "simple-host")
    if dep["spec"]["replicas"] != 3:
        raise AssertionError(f"replicas {dep['spec']['replicas']}")
    c = dep["spec"]["template"]["spec"]["containers"][0]
    if c["resources"]["requests"]["cpu"] != "500m":
        raise AssertionError(f"resources {c['resources']}")
    pod = dep["spec"]["template"]["spec"]
    if pod.get("nodeSelector") != {"disk": "ssd"}:
        raise AssertionError(f"nodeSelector {pod.get('nodeSelector')}")
    tols = pod.get("tolerations") or []
    if not any(t.get("key") == "dedicated" and t.get("operator") == "Exists" for t in tols):
        raise AssertionError(f"tolerations {tols}")
    pulls = pod.get("imagePullSecrets") or []
    if not any(s.get("name") == "regcred" for s in pulls):
        raise AssertionError(f"imagePullSecrets {pulls}")
    env_from = [e.get("secretRef", {}).get("name") for e in c.get("envFrom") or [] if e.get("secretRef")]
    if "simple-host-secrets" not in env_from:
        raise AssertionError(f"server secretRef {env_from}")
    sa = one(by, "ServiceAccount", "simple-host")
    if sa.get("automountServiceAccountToken") is not False:
        raise AssertionError(f"automountServiceAccountToken {sa.get('automountServiceAccountToken')}")
    np = one(by, "NetworkPolicy", "simple-host")
    froms = np["spec"]["ingress"][0]["from"][0]["namespaceSelector"]["matchLabels"]
    if froms.get("kubernetes.io/metadata.name") != "ingress-ns":
        raise AssertionError(f"networkPolicy {froms}")
    one(by, "Deployment", "simple-host-owner-hosts")
    img = c["image"]
    digest = "sha256:e7b6defaa967a58c549c8cf7694bac7be68a15c7e3b419f85d9c1ca3d00e9d9a"
    if not img.endswith("@" + digest):
        raise AssertionError(f"image not pinned to v0.9.3 digest: {img}")
    repo = img.split("@")[0]
    if ":" in repo.rsplit("/", 1)[-1]:
        raise AssertionError(f"image pulled by tag: {img}")


expect(
    "external: DB host/port, existing issuer, extraConfig, scheduling",
    render(
        "external",
        configured
        + [
            "--set",
            "extraConfig.MAX_ARCHIVE_BYTES=209715200",
            "--set",
            "extraConfig.OIDC_EMAIL_CLAIM=preferred_username",
            "--set",
            "extraSecrets.SMTP_URL=smtp://mail.example.com:587",
            "--set",
            "storage.sse=aws:kms",
            "--set",
            "storage.sseKeyId=arn:aws:kms:eu-west-1:1:key/abc",
            "--set",
            "replicas=3",
            "--set",
            "resources.requests.cpu=500m",
            "--set",
            "nodeSelector.disk=ssd",
            "--set",
            "tolerations[0].key=dedicated",
            "--set",
            "tolerations[0].operator=Exists",
            "--set",
            "image.pullSecrets[0]=regcred",
            "--set",
            "networkPolicy.ingressNamespace=ingress-ns",
            "--set",
            "trustedProxyCIDRs=10.0.0.0/8",
        ],
    ),
    external,
)


def incluster(docs, by):
    data = cm(by)
    if data.get("DB_PORT") != "5432":
        raise AssertionError(f"DB_PORT {data.get('DB_PORT')}")
    if data.get("DB_INCLUSTER_EVALUATION") != "true":
        raise AssertionError(f"DB_INCLUSTER_EVALUATION {data.get('DB_INCLUSTER_EVALUATION')}")
    if not str(data.get("DB_HOST", "")).startswith("postgres.simple-host.svc"):
        raise AssertionError(f"DB_HOST {data.get('DB_HOST')}")
    one(by, "StatefulSet", "postgres")
    one(by, "Certificate", "postgres-server")
    absent(by, "Secret", "simple-host-db-ca")
    ing = one(by, "Ingress", "simple-host")
    if (ing.get("spec") or {}).get("ingressClassName"):
        raise AssertionError(
            f"empty className still set ingressClassName {(ing.get('spec') or {}).get('ingressClassName')}"
        )


expect(
    "incluster: StatefulSet, evaluation flag, cluster-default IngressClass",
    render(
        "incluster",
        configured
        + [
            "--set",
            "postgres.mode=incluster",
            "--set",
            "ingress.className=",
            "--set",
            "postgres.external.host=",
            "--set",
            "postgres.external.caCert=",
        ],
    ),
    incluster,
)


def existing_secret(docs, by):
    absent(by, "Secret", "simple-host-secrets")
    one(by, "Secret", "simple-host-db-ca")
    dep = one(by, "Deployment", "simple-host")
    c = dep["spec"]["template"]["spec"]["containers"][0]
    names = [e["secretRef"]["name"] for e in c.get("envFrom") or [] if e.get("secretRef")]
    if names != ["from-vault"]:
        raise AssertionError(f"server envFrom secretRef {names}")
    init = dep["spec"]["template"]["spec"]["initContainers"][0]
    refs = [
        (e.get("valueFrom") or {}).get("secretKeyRef") or {}
        for e in init.get("env") or []
    ]
    if not any(p.get("name") == "from-vault" and p.get("key") == "DB_APP_PASSWORD" for p in refs):
        raise AssertionError(f"migrate secretKeyRef {refs}")
    oh = one(by, "Deployment", "simple-host-owner-hosts")
    ohc = oh["spec"]["template"]["spec"]["containers"][0]
    oh_env = [
        (e.get("valueFrom") or {}).get("secretKeyRef") or {}
        for e in ohc.get("env") or []
    ]
    if not any(p.get("name") == "from-vault" and p.get("key") == "DB_APP_PASSWORD" for p in oh_env):
        raise AssertionError(f"owner-hosts secretKeyRef {oh_env}")
    cron = one(by, "CronJob", "simple-host-prune")
    pc = cron["spec"]["jobTemplate"]["spec"]["template"]["spec"]["containers"][0]
    prefs = [
        (e.get("valueFrom") or {}).get("secretKeyRef") or {}
        for e in pc.get("env") or []
    ]
    if not any(p.get("name") == "from-vault" for p in prefs):
        raise AssertionError(f"prune secretKeyRef {prefs}")


expect(
    "existingSecret: no generated Secret, pods mount from-vault",
    render(
        "existing-secret",
        configured
        + [
            "--set",
            "secrets.existingSecret=from-vault",
            "--set",
            "oidc.clientSecret=",
            "--set",
            "storage.accessKeyId=",
            "--set",
            "storage.secretAccessKey=",
            "--set",
            "postgres.password=",
            "--set",
            "postgres.appPassword=",
        ],
    ),
    existing_secret,
)


def digitalocean(docs, by):
    data = cm(by)
    if data.get("BACKUP_STORAGE_ENDPOINT") != "https://nyc3.digitaloceanspaces.com":
        raise AssertionError(f"endpoint {data.get('BACKUP_STORAGE_ENDPOINT')}")
    if data.get("BACKUP_STORAGE_REGION") != "nyc3":
        raise AssertionError(f"region {data.get('BACKUP_STORAGE_REGION')}")
    if data.get("BACKUP_SSE") != "none":
        raise AssertionError(f"sse {data.get('BACKUP_SSE')}")
    if str(data.get("DB_PORT")) != "25060":
        raise AssertionError(f"DO managed PG port {data.get('DB_PORT')}")
    if data.get("OWNER_CERT_ISSUER") != "simple-host-letsencrypt":
        raise AssertionError(f"OWNER_CERT_ISSUER {data.get('OWNER_CERT_ISSUER')}")
    ing = one(by, "Ingress", "simple-host")
    if (ing.get("spec") or {}).get("ingressClassName") != "simple-host":
        raise AssertionError(f"className {(ing.get('spec') or {}).get('ingressClassName')}")
    iss = one(by, "ClusterIssuer", "simple-host-letsencrypt")
    solvers = iss["spec"]["acme"]["solvers"]
    if not solvers or "digitalocean" not in (solvers[0].get("dns01") or {}):
        raise AssertionError(f"issuer solvers {solvers}")
    if solvers[0]["selector"]["dnsZones"] != ["sites.example.com"]:
        raise AssertionError(f"dnsZones {solvers[0]['selector']}")
    np = one(by, "NetworkPolicy", "simple-host")
    froms = np["spec"]["ingress"][0]["from"][0]["namespaceSelector"]["matchLabels"]
    if froms.get("kubernetes.io/metadata.name") != "simple-host-ingress":
        raise AssertionError(f"DO ingress namespace {froms}")


expect("digitalocean preset: Spaces, Traefik class, DO DNS-01 issuer, port 25060", render("digitalocean", do_preset), digitalocean)


def manual(docs, by):
    data = cm(by)
    if data.get("OWNER_CERTS") != "manual":
        raise AssertionError(f"OWNER_CERTS {data.get('OWNER_CERTS')}")
    if "OWNER_CERT_ISSUER" in data:
        raise AssertionError("manual still set OWNER_CERT_ISSUER")
    absent(by, "Deployment", "simple-host-owner-hosts")


expect(
    "ownerCerts=manual: no reconciler",
    render("manual", configured + ["--set", "certificates.ownerCerts=manual"]),
    manual,
)


def manual_no_issuer(docs, by):
    data = cm(by)
    if data.get("OWNER_CERTS") != "manual":
        raise AssertionError(f"OWNER_CERTS {data.get('OWNER_CERTS')}")
    if "OWNER_CERT_ISSUER" in data:
        raise AssertionError("manual still set OWNER_CERT_ISSUER")
    absent(by, "Deployment", "simple-host-owner-hosts")
    ing = one(by, "Ingress", "simple-host")
    anns = (ing.get("metadata") or {}).get("annotations") or {}
    if "cert-manager.io/cluster-issuer" in anns:
        raise AssertionError("manual empty issuer still annotated cluster-issuer")
    tls = (ing.get("spec") or {}).get("tls") or []
    if not tls or tls[0].get("secretName") != "simple-host-tls":
        raise AssertionError(f"tlsSecret {tls}")


expect(
    "ownerCerts=manual empty issuer: no cluster-issuer annotation, existing tlsSecret",
    render(
        "manual-no-issuer",
        configured
        + [
            "--set",
            "certificates.ownerCerts=manual",
            "--set",
            "certificates.issuer=",
        ],
    ),
    manual_no_issuer,
)


def http_override(docs, by):
    data = cm(by)
    if data.get("BACKUP_STORAGE_ENDPOINT") != "http://minio.minio.svc:9000":
        raise AssertionError(f"endpoint {data.get('BACKUP_STORAGE_ENDPOINT')}")
    if str(data.get("BACKUP_STORAGE_INSECURE_ALLOWED")).lower() != "true":
        raise AssertionError(
            f"BACKUP_STORAGE_INSECURE_ALLOWED {data.get('BACKUP_STORAGE_INSECURE_ALLOWED')}"
        )


expect(
    "http endpoint with extraConfig.BACKUP_STORAGE_INSECURE_ALLOWED=true",
    render(
        "http-insecure",
        configured
        + [
            "--set",
            "storage.endpoint=http://minio.minio.svc:9000",
            "--set",
            "extraConfig.BACKUP_STORAGE_INSECURE_ALLOWED=true",
        ],
    ),
    http_override,
)


def identity(docs, by):
    sa = one(by, "ServiceAccount", "simple-host")
    if sa.get("automountServiceAccountToken") is not False:
        raise AssertionError(f"automount {sa.get('automountServiceAccountToken')}")
    anns = (sa.get("metadata") or {}).get("annotations") or {}
    if anns.get("eks.amazonaws.com/role-arn") != "arn:aws:iam::123:role/simple-host":
        raise AssertionError(f"SA annotations {anns}")
    dep = one(by, "Deployment", "simple-host")
    pod_meta = dep["spec"]["template"]["metadata"]
    labels = pod_meta.get("labels") or {}
    if labels.get("team") != "platform":
        raise AssertionError(f"pod labels {labels}")
    if labels.get("app") != "simple-host":
        raise AssertionError("app label missing or overwritten")
    pod_anns = pod_meta.get("annotations") or {}
    if pod_anns.get("cluster-autoscaler.kubernetes.io/safe-to-evict") != "false":
        raise AssertionError(f"pod annotations {pod_anns}")
    if "checksum/config" not in pod_anns:
        raise AssertionError("checksum/config missing")
    oh_sa = one(by, "ServiceAccount", "simple-host-owner-hosts")
    oh_anns = (oh_sa.get("metadata") or {}).get("annotations") or {}
    if "eks.amazonaws.com/role-arn" in oh_anns:
        raise AssertionError("owner-hosts SA received identity annotations")
    oh = one(by, "Deployment", "simple-host-owner-hosts")
    oh_pod = oh["spec"]["template"]["metadata"]
    if (oh_pod.get("labels") or {}).get("team") == "platform":
        raise AssertionError("owner-hosts pod received podLabels")
    if "cluster-autoscaler.kubernetes.io/safe-to-evict" in (oh_pod.get("annotations") or {}):
        raise AssertionError("owner-hosts pod received podAnnotations")


expect(
    "identity: SA/pod annotations and labels on app only, token unmounted",
    render(
        "identity",
        configured
        + [
            "--set-json",
            'serviceAccount.annotations={"eks.amazonaws.com/role-arn":"arn:aws:iam::123:role/simple-host"}',
            "--set-json",
            'podAnnotations={"cluster-autoscaler.kubernetes.io/safe-to-evict":"false"}',
            "--set-json",
            'podLabels={"team":"platform"}',
        ],
    ),
    identity,
)


refuse("configured without issuer", "certificates.issuer is required", configured + ["--set", "certificates.issuer="])
refuse(
    "configured without storage.endpoint",
    "storage.endpoint is required",
    configured + ["--set", "storage.endpoint="],
)
refuse(
    "http storage.endpoint",
    "storage.endpoint must be an https URL",
    configured + ["--set", "storage.endpoint=http://s3.example.com"],
)
refuse(
    "ftp storage.endpoint even with insecure override",
    "storage.endpoint must be an https URL",
    configured
    + [
        "--set",
        "storage.endpoint=ftp://s3.example.com",
        "--set",
        "extraConfig.BACKUP_STORAGE_INSECURE_ALLOWED=true",
    ],
)
refuse(
    "external without postgres.password",
    "postgres.password is required",
    configured + ["--set", "postgres.password="],
)
refuse(
    "configured without storage.region",
    "storage.region is required",
    configured + ["--set", "storage.region="],
)
refuse(
    "external without host",
    "postgres.external.host is required",
    configured + ["--set", "postgres.external.host="],
)
refuse(
    "external verify-full without caCert",
    "postgres.external.caCert is required",
    configured + ["--set", "postgres.external.caCert="],
)
refuse(
    "Google without allowedEmailDomains",
    "oidc.allowedEmailDomains is required with Google",
    configured
    + [
        "--set",
        "oidc.issuer=https://accounts.google.com",
        "--set",
        "oidc.allowedEmailDomains=",
    ],
)
refuse(
    "createIssuer without DigitalOcean token",
    "digitaloceanToken",
    configured
    + [
        "--set",
        "certificates.createIssuer=true",
        "--set",
        "certificates.issuer=simple-host-letsencrypt",
        "--set",
        "certificates.acme.email=ops@example.com",
    ],
)
refuse(
    "extraConfig owns a chart key",
    "extraConfig.DB_HOST",
    configured + ["--set", "extraConfig.DB_HOST=elsewhere"],
)
refuse(
    "extraConfig secret-looking key",
    "extraConfig.OIDC_CLIENT_SECRET looks like a secret",
    configured + ["--set", "extraConfig.OIDC_CLIENT_SECRET=x"],
)
refuse(
    "extraSecrets with existingSecret",
    "extraSecrets cannot be used with secrets.existingSecret",
    configured
    + [
        "--set",
        "secrets.existingSecret=from-vault",
        "--set",
        "extraSecrets.SMTP_URL=smtp://mail.example.com:587",
    ],
)
refuse(
    "sse none with envelopeKey -",
    'storage.envelopeKey "-" needs storage.sse AES256 or aws:kms',
    configured + ["--set", "storage.sse=none", "--set", "storage.envelopeKey=-"],
)
refuse(
    "aws:kms without sseKeyId",
    "storage.sseKeyId is required with storage.sse=aws:kms",
    configured + ["--set", "storage.sse=aws:kms"],
)
refuse(
    "image digest not sha256",
    "image.digest must be a sha256 digest",
    configured + ["--set", "image.digest=v0.9.2"],
)
refuse(
    "postgres.mode garbage",
    "postgres.mode must be incluster or external",
    configured + ["--set", "postgres.mode=rds"],
)

if fail:
    print("test-chart: FAILED")
    sys.exit(1)
print("test-chart: ok")
PY
