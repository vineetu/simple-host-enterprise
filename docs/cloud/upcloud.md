# UpCloud (UKS)

Verified: v1.0.0 install and v1.1.0 upgrade, 2026-09-25. UpCloud Kubernetes Service
(Kubernetes 1.34, two workers), ingress-nginx behind the UKS load balancer, Managed
PostgreSQL 16 (1 GB plan), Managed Object Storage. After the upgrade (the
`docs/storage.md` migration: 4 versions and 2 assets moved, 0 failed) `make smoke`
passed 37 of 37, and 38 of 38 with a second person's key.

Fills the sections in [README.md](README.md).

## 1. Cluster & ingress

- **API allow-list.** A new UKS cluster's API admits no addresses, and `kubectl` fails
  with `EOF` or a timeout rather than an auth error. Add the address you run `kubectl`
  from: `upctl kubernetes modify <cluster> --kubernetes-api-allow-ip <your IP>`.
- **Ingress.** There is no managed ingress controller, so `make prereqs` installs
  ingress-nginx and the UpCloud cloud controller gives its Service a load balancer.
  ingress-nginx is retired upstream (maintenance ended March 2026).
- **The load balancer must pass TCP through, with PROXY protocol.** By default the
  controller makes port 443 an HTTP-mode frontend that terminates TLS itself, so every
  host is served the load balancer's own certificate. In TCP mode alone, every request
  arrives from the load balancer's private address, which collapses the per-address
  rate limits and makes every IP in the access log the same. Annotate the ingress-nginx
  Service:

  ```yaml
  service.beta.kubernetes.io/upcloud-load-balancer-config: '{"frontends":[{"name":"http","mode":"tcp"},{"name":"https","mode":"tcp"}],"backends":[{"name":"http","properties":{"outbound_proxy_protocol":"v1"}},{"name":"https","properties":{"outbound_proxy_protocol":"v1"}}]}'
  ```

  and turn PROXY protocol on in ingress-nginx's ConfigMap
  (`ingress-nginx-controller`): `use-proxy-protocol: "true"`. Set both together: either
  one without the other breaks every request.
- The load balancer takes about four minutes to get its hostname after the Service
  exists.
- **Pod addresses.** UKS runs Cilium in cluster-pool mode: pods get addresses from
  `192.168.0.0/16`, not from the `10.244.x.0/24` a node's `.spec.podCIDR` shows. Set
  `TRUSTED_PROXY_CIDRS=192.168.0.0/16`, or whatever range
  `kubectl -n ingress-nginx get pod -o wide` shows.

## 2. DNS & wildcard certificate

`CNAME` records for `<base>` and `*.<base>` pointing at the load balancer's hostname
(`lb-….upcloudlb.com`), wherever the zone lives.

The wildcard needs DNS-01 with your DNS provider: a cert-manager solver or webhook for
that provider, or a certificate issued out of band (for example certbot with the
provider's DNS hooks) into the `simple-host-tls` Secret, with the cert-manager
annotation removed from the Ingress.

## 3. Postgres

Managed PostgreSQL 16. TLS is on.

- **Port 11569, not 5432.** Set `DB_PORT=11569`.
- **IP filter.** A new database admits no addresses. Add the worker nodes' public IPs
  (and yours, for the checks in `INSTALL.md` section 2).
- **Owning role.** The admin user's (`upadmin`) password cannot be chosen at creation,
  so create the owning role yourself, as `upadmin`, with the `DB_PASSWORD` from
  `secrets.env`:

  ```sql
  CREATE ROLE simplehost LOGIN CREATEROLE PASSWORD '<DB_PASSWORD>';
  CREATE DATABASE simplehost OWNER simplehost;
  ```

- **CA.** The server certificate is signed by the project's own CA. Download it from
  the database's page in the UpCloud console and save it as
  `deploy/overlays/byo/db-ca.crt`.
- `upctl` rejects `--property version=16` at creation; create the database in the
  console or through the API.
- Backups: check the plan's backup retention covers 7+ days. The verified run did not
  test a restore.
- **Connections.** The 1 GB plan allows 50 connections. Two replicas plus the rollout's
  extra pod can open 60 (`docs/storage.md`); the verified run stayed far below that,
  but under load use a larger plan.

```
DB_HOST=<the database's public hostname>
DB_PORT=11569
DB_SSLMODE=verify-full
DB_SSL_ROOT_CERT=/etc/simple-host/db-ca/ca.crt
```

## 4. Bucket

Managed Object Storage. Create a user with an access key and a policy that can manage
the bucket (the verified run used `ECSS3FullAccess`; narrow it to the one bucket if you
can), then the bucket, with versioning on and the lifecycle rule through any S3 client (the
`curl` commands in `docs/storage.md` work; UpCloud accepts the same XML as AWS).
`x-amz-server-side-encryption: AES256` is accepted.

```
BACKUP_STORAGE_ENDPOINT=https://<your endpoint>.upcloudobjects.com
BACKUP_STORAGE_REGION=<region, e.g. us-1>
BACKUP_STORAGE_BUCKET=<bucket>
```

**Checksums.** The AWS SDK's default request checksums are refused by UpCloud Object
Storage (`400 XAmzContentSHA256Mismatch`), and on v1.0.0 the failure shows only in the
pod log. On v1.0.0, add to `config.env`:

```
AWS_REQUEST_CHECKSUM_CALCULATION=when_required
AWS_RESPONSE_CHECKSUM_VALIDATION=when_required
```

The server asks for checksums only when required on any non-AWS endpoint, so these
lines are not needed.

## 5. Identity for pods

There is no workload identity for Object Storage: put the user's access key pair in
`BACKUP_STORAGE_ACCESS_KEY_ID` / `BACKUP_STORAGE_SECRET_ACCESS_KEY`.

Upgrading from v1.0.0, which kept site data on a volume: follow `docs/storage.md`,
"Migrating from a PVC install", then delete the old volume by hand. Every UpCloud
StorageClass has `reclaimPolicy: Retain`, so the disk outlives its PVC and the cluster
and keeps billing until you delete the released PersistentVolume and the storage in
the UpCloud console.

## 6. OIDC provider notes

- Worker egress to the issuer on 443 is open by default.
- **Okta:** set client authentication to "Client secret" with `client_secret_post`.
- **Entra ID:** single-tenant app, issuer `https://login.microsoftonline.com/<tenant-id>/v2.0`.

## 7. Verify

```
kubectl --context "$CTX" -n simple-host rollout status deploy/simple-host
curl -fsS https://<base>/readyz
kubectl --context "$CTX" -n simple-host run oidc-check --rm -i --restart=Never --image=curlimages/curl -- curl -fsS https://<issuer>/.well-known/openid-configuration
make smoke BASE=https://<base> KEY_FILE=<path-to-admin-api-key>
```

Also check that the certificate `https://<base>` presents is yours, not the load
balancer's (`curl -v` shows the subject), and that the request log shows real client
addresses, not the load balancer's private one.
