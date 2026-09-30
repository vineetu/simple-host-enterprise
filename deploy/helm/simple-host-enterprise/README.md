# simple-host-enterprise Helm chart

Simple Host Enterprise on any Kubernetes cluster, with defaults for DigitalOcean
Kubernetes. Needs cert-manager and an ingress controller in the cluster.

```sh
helm install simple-host-enterprise oci://ghcr.io/vineetu/charts/simple-host-enterprise --version 0.1.0 -n simple-host --create-namespace -f my-values.yaml
```

- Every value, with its default and what it does: [values.yaml](values.yaml).
- The walkthrough (DNS, certificates, database, bucket, sign-in):
  [docs/cloud/digitalocean.md](../../../docs/cloud/digitalocean.md). On another cloud,
  change the bucket endpoint, `storage.sse`, `ingress.className`,
  `networkPolicy.ingressNamespace` and the ClusterIssuer.
- Label the namespace for the restricted Pod Security profile. Every pod meets it:
  `kubectl label ns simple-host pod-security.kubernetes.io/enforce=restricted`.
- Until `host`, `oidc.issuer`, `oidc.clientId` and `storage.bucket` are set, only the
  database and the generated secrets are installed.
- Rendered without cluster access (`helm template`, Argo CD), the chart cannot read
  back the secrets it generated and would make new ones: use `secrets.existingSecret`
  there.
- Every value, secrets included, is kept in Helm's release history. For the OIDC
  secret, the bucket keys and the DigitalOcean token, `secrets.existingSecret` and
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
