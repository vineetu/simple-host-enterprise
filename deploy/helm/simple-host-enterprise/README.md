# simple-host-enterprise Helm chart

Simple Host Enterprise on an existing Kubernetes cluster. Needs an ingress
controller already in the cluster. cert-manager is required with
`certificates.ownerCerts: auto` (the default), `certificates.createIssuer`,
and `postgres.mode: incluster`. With `ownerCerts: manual` and an existing
`ingress.tlsSecret`, the application Ingress does not request a certificate.
It does not create a cluster, VPC, or cloud account resources.

```sh
helm install simple-host-enterprise oci://ghcr.io/vineetu/charts/simple-host-enterprise --version 0.2.1 -n simple-host --create-namespace -f my-values.yaml
```

- Every value, with its default and what it does: [values.yaml](values.yaml).
- Generic walkthrough (existing Postgres, bucket, issuer, OIDC, `helm template`):
  [docs/install-kubernetes.md](../../../docs/install-kubernetes.md).
- DigitalOcean Spaces, Traefik class `simple-host`, and a Let's Encrypt DNS-01
  issuer on DigitalOcean DNS: pass
  [values-digitalocean.yaml](values-digitalocean.yaml) as well
  (`-f deploy/helm/simple-host-enterprise/values-digitalocean.yaml` from a
  clone; [docs/cloud/digitalocean.md](../../../docs/cloud/digitalocean.md)).
  Chart 0.1.1 still has those as defaults; marketplace workflows can pin it.
- Label the namespace for the restricted Pod Security profile. Every pod meets it:
  `kubectl label ns simple-host pod-security.kubernetes.io/enforce=restricted`.
- Until `host`, `oidc.issuer`, `oidc.clientId` and `storage.bucket` are set, only the
  in-cluster database (when `postgres.mode` is `incluster`) and the generated secrets
  are installed.
- Rendered without cluster access (`helm template`, Argo CD), the chart cannot read
  back the secrets it generated and would make new ones: create the namespace and a
  persistent Secret first, then set `secrets.existingSecret`. Redirect to
  `simple-host.yaml` and `kubectl apply -n simple-host -f` it
  (`docs/install-kubernetes.md`). With `postgres.mode: external`, `DB_PASSWORD` in
  that Secret is the existing owning role's password, never generated.
- `serviceAccount.annotations`, `podAnnotations` and `podLabels` attach an existing
  platform-managed identity to the app ServiceAccount and pod. The AWS SDK default
  credential chain supports AWS-compatible mechanisms only; other S3 providers use
  static HMAC keys. Owner-hosts does not receive these annotations. The API token
  stays unmounted.
- Every value, secrets included, is kept in Helm's release history. For the OIDC
  secret, the bucket keys and a DigitalOcean DNS token, `secrets.existingSecret` and
  `certificates.acme.digitaloceanTokenSecret` keep them out of it.

## Upgrade

A new release is a new chart version, with the new image digest:

```sh
helm upgrade simple-host-enterprise oci://ghcr.io/vineetu/charts/simple-host-enterprise --version <new> -n simple-host --reset-then-reuse-values
```

`--reset-then-reuse-values` (Helm 3.14 or later) keeps your settings and takes the new
chart's defaults. `--reuse-values` would keep the old image.

## Remove

`helm uninstall` keeps `simple-host-secrets` (it holds the envelope key) and the
database volume, so a reinstall under the same release name picks both up again.
Deleting the namespace removes both; the bucket and a managed database are never
touched. The DigitalOcean 1-Click's uninstall deletes the namespace: back the envelope
key up first.

## Publishing

Push a tag `chart-v<version>` matching `Chart.yaml`, on a commit in `main`
(`.github/workflows/chart.yml`). A published version is never replaced.

Chart 0.2.1 pins release 0.9.3: personal home selection, signed-in showcase
feed, bio, pins and manual order. Site access and origins remain in force. Set
`extraConfig.SHOWCASE_BIO_MAX_LENGTH` to change the default 280-character bio
limit. [Home-page behavior](../../../docs/your-home-page.md).
