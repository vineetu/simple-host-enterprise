# simple-host-enterprise Helm chart

Simple Host Enterprise on any Kubernetes cluster, with defaults for DigitalOcean
Kubernetes. Needs cert-manager and an ingress controller in the cluster.

```sh
helm install simple-host oci://ghcr.io/vineetu/charts/simple-host-enterprise -n simple-host --create-namespace -f my-values.yaml
```

- Every value, with its default and what it does: [values.yaml](values.yaml).
- The walkthrough (DNS, certificates, database, bucket, sign-in):
  [docs/cloud/digitalocean.md](../../../docs/cloud/digitalocean.md). The same values work
  on other clouds with their own bucket endpoint, `storage.sse` and ClusterIssuer.
- Label the namespace for the restricted Pod Security profile; every pod meets it:
  `kubectl label ns simple-host pod-security.kubernetes.io/enforce=restricted`.
- Until `host`, `oidc.issuer`, `oidc.clientId` and `storage.bucket` are set, only the
  database and the generated secrets are installed.
- Publishing: push a tag `chart-v<version>` matching `Chart.yaml`
  (`.github/workflows/chart.yml`).
