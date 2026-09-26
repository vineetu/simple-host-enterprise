# Azure (AKS)

Not yet run end to end on this cloud.

Fills the sections in [README.md](README.md).

## 1. Cluster & ingress

Use the application routing add-on (managed NGINX):
`az aks approuting enable --resource-group <rg> --name <cluster>`. Then set
`spec.ingressClassName: webapprouting.kubernetes.azure.com` in `ingress-patch.yaml`.
The `nginx.ingress.kubernetes.io/*` lines already in that file (body size, timeouts,
redirect) apply as written.

- Azure supports the add-on's managed NGINX until November 2026. After that, its
  successor is the add-on's Gateway API implementation. This package ships an Ingress.
- Create the cluster with `--enable-oidc-issuer --enable-workload-identity`
  (section 5).
- To pull from a private registry, run `az aks update --resource-group <rg> --name <cluster> --attach-acr <registry>`,
  or put a pull secret on the `simple-host` ServiceAccount. The pattern is in the comments of
  `deploy/overlays/byo/kustomization.yaml`.

## 2. DNS & wildcard certificate

Azure DNS `A` records for `<base>` and `*.<base>` point at the add-on's load balancer IP.

Use cert-manager with Azure DNS DNS-01 and workload identity:

```yaml
solvers:
  - dns01:
      azureDNS:
        subscriptionID: <subscription-id>
        resourceGroupName: <dns-zone-rg>
        hostedZoneName: <zone>
        environment: AzurePublicCloud
        managedIdentity: {clientID: <identity-client-id>}
```

1. Create a user-assigned identity and grant it **DNS Zone Contributor** on the one zone.
2. Federate it to cert-manager's ServiceAccount (one line):
   `az identity federated-credential create --name cert-manager --identity-name <identity> --resource-group <rg> --issuer "$(az aks show -g <rg> -n <cluster> --query oidcIssuerProfile.issuerUrl -o tsv)" --subject system:serviceaccount:cert-manager:cert-manager`
3. Label the cert-manager pods `azure.workload.identity/use: "true"`
   (Helm values: `podLabels: {azure.workload.identity/use: "true"}`).

Owner certificates (`*.<owner>.<base>`) work with the add-on's NGINX as written: the
reconciler's per-owner Ingresses copy its class and annotations. Name this issuer, or
an internal CA issuer (no rate limits, no DNS), in `OWNER_CERT_ISSUER`.

## 3. Postgres

Azure Database for PostgreSQL flexible server, version 16.

Some subscriptions refuse to create one. Free-trial and Azure-program subscriptions
fail with "The location is restricted from performing this operation". Registering the
resource provider does not fix it. A quota support request for region access does, or
moving to pay-as-you-go.

- Extensions: the migrations create `pgcrypto`, so allow-list it:
  `az postgres flexible-server parameter set --resource-group <rg> --server-name <server> --name azure.extensions --value PGCRYPTO`
- TLS: `require_secure_transport` must be `on` (the default).
- PITR: backup retention 7+ days (`--backup-retention 7` or higher).
- Encryption at rest: always on for flexible server (service-managed key by default;
  a customer-managed key can only be chosen at creation).
- CA bundle: concatenate the roots. DigiCert Global Root CA is only needed while the
  G1-to-G2 root rotation is still running in your region.

```
curl -fsS https://cacerts.digicert.com/DigiCertGlobalRootG2.crt.pem > deploy/overlays/byo/db-ca.crt
curl -fsS "https://www.microsoft.com/pkiops/certs/Microsoft%20RSA%20Root%20Certificate%20Authority%202017.crt" | openssl x509 -inform DER >> deploy/overlays/byo/db-ca.crt
curl -fsS https://cacerts.digicert.com/DigiCertGlobalRootCA.crt.pem >> deploy/overlays/byo/db-ca.crt
```

```
DB_HOST=<server>.postgres.database.azure.com
DB_SSLMODE=verify-full
DB_SSL_ROOT_CERT=/etc/simple-host/db-ca/ca.crt
```

With private access, the private DNS zone must resolve that same name, or `verify-full` fails.

## 4. Bucket

This package has no native Azure Blob backend yet. Azure needs an S3-compatible object
store over HTTPS until one ships. MinIO's gateway mode is gone upstream, so do not plan on
putting it in front of Blob Storage. Whatever store you choose must support versioning, a
lifecycle rule that expires noncurrent versions, and encryption at rest.

```
BACKUP_STORAGE_ENDPOINT=https://<s3-compatible-endpoint>
BACKUP_STORAGE_REGION=<region the store expects>
BACKUP_STORAGE_BUCKET=<bucket>
```

## 5. Identity for pods

The server's S3 client uses the AWS SDK credential chain, which does not read Azure workload
identity. Put the store's static key pair in `BACKUP_STORAGE_ACCESS_KEY_ID` /
`BACKUP_STORAGE_SECRET_ACCESS_KEY`. Workload identity is used by cert-manager (section 2).

## 6. OIDC provider notes

- **Entra ID:** register the app single-tenant ("Accounts in this organizational directory
  only"). Use the issuer `https://login.microsoftonline.com/<tenant-id>/v2.0`, never
  `common` or `organizations`, because a multi-tenant app admits accounts from other tenants.
- **Okta:** set client authentication to "Client secret" with `client_secret_post`.
- The pods need egress on 443 to the issuer.

## 7. Verify

```
kubectl -n simple-host rollout status deploy/simple-host
curl -fsS https://<base>/healthz
kubectl -n simple-host run oidc-check --rm -i --restart=Never --image=curlimages/curl -- curl -fsS https://<issuer>/.well-known/openid-configuration
make smoke BASE=https://<base> KEY_FILE=<path-to-admin-api-key>
```
