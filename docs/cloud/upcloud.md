# UpCloud (UKS)

Verified: v1.0.0 install and v1.1.0 upgrade, 2026-09-25. UpCloud Kubernetes Service
(Kubernetes 1.34, two workers), ingress-nginx behind the UKS load balancer, Managed
PostgreSQL 16 (1 GB plan), Managed Object Storage. After the upgrade (the
`docs/storage.md` migration: 4 versions and 2 assets moved, 0 failed) `make smoke`
passed 37 of 37, and 38 of 38 with a second person's key. v1.8.0 fresh install,
2026-09-27: Kubernetes 1.35 (one `DEV-2xCPU-4GB` node), PostgreSQL 16.15, a bucket-only
IAM policy, `BACKUP_ENVELOPE_KEY` on, an internal CA for owner certificates; `make smoke`
passed 45 of 45 with a Full key and a second person's key.

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
  one without the other breaks every request. Do it **straight after `make prereqs`**,
  before the load balancer finishes provisioning: `prereqs` creates the Service in HTTP
  mode, and the annotation is applied while the load balancer is still being built.

  ```sh
  kubectl --context "$CTX" -n ingress-nginx annotate svc ingress-nginx-controller --overwrite 'service.beta.kubernetes.io/upcloud-load-balancer-config={"frontends":[{"name":"http","mode":"tcp"},{"name":"https","mode":"tcp"}],"backends":[{"name":"http","properties":{"outbound_proxy_protocol":"v1"}},{"name":"https","properties":{"outbound_proxy_protocol":"v1"}}]}'
  ```

  ```sh
  kubectl --context "$CTX" -n ingress-nginx patch configmap ingress-nginx-controller --type merge -p '{"data":{"use-proxy-protocol":"true"}}'
  ```
- The load balancer takes four to six minutes to get its hostname after the Service
  exists. A new cluster's API can answer `EOF` for about 30 seconds after the cluster
  reports `running`, even with the allow-list set; retry.
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

Owner certificates (`*.<owner>.<base>`, v1.3.0, not part of the verified run above)
work with ingress-nginx as written: the
reconciler's per-owner Ingresses copy its class and annotations, and cert-manager
issues from `OWNER_CERT_ISSUER`. An internal CA issuer needs no DNS solver and has no
rate limits; certbot out of band does not keep up with new owners, so use
`OWNER_CERTS=manual` only if something else issues them.

## 3. Postgres

Managed PostgreSQL 16. TLS is on. v1.8.0 was also installed on PostgreSQL 16.15
(plan `1x1xCPU-1GB-10GB`, de-fra1) on 2026-09-27; it was ready about four minutes after
creation.

The API calls below read the UpCloud token from a 0600 file holding one line,
`Authorization: Bearer <token>` (`AUTH=<that file>`), so it is never on a command line.

- **Create it through the API.** `upctl` rejects `--property version=16` at creation;
  the console or the API works (put your own address and, see below, the nodes' in
  `ip_filter`):

  ```sh
  curl -fsS -H @"$AUTH" -H 'Content-Type: application/json' -d '{"type":"pg","plan":"1x1xCPU-1GB-10GB","zone":"de-fra1","hostname_prefix":"simple-host","title":"simple-host","properties":{"version":"16","ip_filter":["<your IP>/32"],"public_access":true}}' https://api.upcloud.com/1.3/database -o db.json
  ```

- **`upctl database show` prints the admin password** and the full service URI in
  clear text. Do not run it where its output is kept (an agent's transcript, a CI log).
  Read what you need through the API instead, into variables:

  ```sh
  DB_UUID="$(python3 -c 'import json; print(json.load(open("db.json"))["uuid"])')"; rm -f db.json
  ```

  ```sh
  DB_PUBLIC_HOST="$(curl -fsS -H @"$AUTH" "https://api.upcloud.com/1.3/database/$DB_UUID" | python3 -c 'import json,sys; print(next(c["host"] for c in json.load(sys.stdin)["components"] if c.get("route") == "public"))')"; echo "$DB_PUBLIC_HOST"
  ```

  ```sh
  export PGPASSWORD="$(curl -fsS -H @"$AUTH" "https://api.upcloud.com/1.3/database/$DB_UUID" | python3 -c 'import json,sys; print(json.load(sys.stdin)["service_uri_params"]["password"])')"
  ```

- **Use the `public-` hostname.** The `Host:` that `upctl database show` prints
  (`<name>.db.upclouddatabases.com`) resolves, from outside UpCloud's private network,
  to a private `10.33.x` address. The hostname that works from the cluster and from
  your machine is the `public-<name>…` one, the component whose `route` is `public`
  (`DB_PUBLIC_HOST` above). Use it as `DB_HOST`.
- **Port 11569, not 5432.** Set `DB_PORT=11569`.
- **IP filter.** A new database admits no addresses. Add your own address (for the
  checks in `INSTALL.md` section 2) and the worker nodes' public IPs. **Node IPs change**
  when a node is replaced or the node group scales, and pods then lose the database
  with nothing but connection timeouts in the log. Prefer attaching the database to the
  cluster's private network (the database's network settings), or update the filter
  every time the node group changes.
- **CA.** The server certificate is signed by the project's own CA. Download it from
  the database's page in the UpCloud console, or read it from the server's own TLS
  chain (the last certificate, "<project> Project CA"):

  ```sh
  openssl s_client -starttls postgres -connect "$DB_PUBLIC_HOST:11569" -showcerts </dev/null 2>/dev/null | awk '/BEGIN CERT/{c=""} {c=c $0 "\n"} /END CERT/{last=c} END{printf "%s", last}' > deploy/overlays/byo/db-ca.crt && openssl x509 -in deploy/overlays/byo/db-ca.crt -noout -subject -fingerprint -sha256
  ```

  Compare the printed fingerprint with the CA's on the console before you trust it:
  a certificate read off the network is only as good as that check.
- **Owning role.** The admin user's (`upadmin`) password cannot be chosen at creation,
  so create the owning role yourself, as `upadmin`, with the `DB_PASSWORD` from
  `secrets.env`. With `PGPASSWORD` set as above:

  ```sh
  psql "host=$DB_PUBLIC_HOST port=11569 dbname=defaultdb user=upadmin sslmode=verify-full sslrootcert=deploy/overlays/byo/db-ca.crt" -c 'CREATE ROLE simplehost LOGIN CREATEROLE'
  ```

  ```sh
  python3 scripts/db-role-password.py deploy/overlays/byo/secrets.env DB_PASSWORD simplehost | psql "host=$DB_PUBLIC_HOST port=11569 dbname=defaultdb user=upadmin sslmode=verify-full sslrootcert=deploy/overlays/byo/db-ca.crt"
  ```

  ```sh
  psql "host=$DB_PUBLIC_HOST port=11569 dbname=defaultdb user=upadmin sslmode=verify-full sslrootcert=deploy/overlays/byo/db-ca.crt" -c 'CREATE DATABASE simplehost OWNER simplehost'; unset PGPASSWORD
  ```

  `scripts/db-role-password.py` sends only a SCRAM verifier (a salted hash), like
  `psql`'s `\password`, which needs a terminal to prompt on; the password never
  reaches a command line or the server's statement logs. With a terminal,
  `\password simplehost` in a `psql` session does the same.
- **Encryption at rest.** Confirm with UpCloud that the plan stores the database and
  its backups encrypted at rest; the verified run did not check this.
- Backups: check the plan's backup retention covers 7+ days. The verified run did not
  test a restore.
- **Connections.** The 1 GB plan allows 50 connections. Two replicas plus the rollout's
  extra pod can open 60 (`docs/storage.md`); the verified run stayed far below that,
  but under load use a larger plan.

```
DB_HOST=<the public- hostname, DB_PUBLIC_HOST above>
DB_PORT=11569
DB_SSLMODE=verify-full
DB_SSL_ROOT_CERT=/etc/simple-host/db-ca/ca.crt
```

## 4. Bucket

Managed Object Storage. Create the bucket, a user with an access key, and a policy
that allows only that bucket, then turn on versioning and the lifecycle rule through any
S3 client (the `curl` commands in `docs/storage.md` work; UpCloud accepts the same XML
as AWS). `x-amz-server-side-encryption: AES256` is accepted. Do not attach the built-in
`ECSS3FullAccess`: it reaches every bucket of the service.

`upctl` has no commands for IAM policies; use the API (`AUTH` as in section 3,
`SVC` the Object Storage service's UUID, `<user>` the user the access key belongs to):

```sh
POLICY="$(python3 -c 'import json,sys,urllib.parse; b=sys.argv[1]; print(urllib.parse.quote(json.dumps({"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*"],"Resource":["arn:aws:s3:::"+b,"arn:aws:s3:::"+b+"/*"]}]})))' '<bucket>')"; curl -fsS -H @"$AUTH" -H 'Content-Type: application/json' -d "{\"name\":\"simple-host-bucket\",\"description\":\"simple-host: one bucket only\",\"document\":\"$POLICY\"}" "https://api.upcloud.com/1.3/object-storage-2/$SVC/policies"
```

```sh
curl -fsS -H @"$AUTH" -H 'Content-Type: application/json' -d '{"name":"simple-host-bucket"}' "https://api.upcloud.com/1.3/object-storage-2/$SVC/users/<user>/policies"
```

The v1.8.0 run on 2026-09-27 used exactly this narrow policy, with `BACKUP_ENVELOPE_KEY`
on. The `docs/storage.md` bucket commands read the key pair from a 0600 curl config
file (`-K`), never from the command line.

```
BACKUP_STORAGE_ENDPOINT=https://<your endpoint>.upcloudobjects.com
BACKUP_STORAGE_REGION=<the region the bucket lives in, e.g. europe-2>
BACKUP_STORAGE_BUCKET=<bucket>
```

The region is the Object Storage service's (the endpoint's region, such as `europe-2`
or `us-1`), not the cluster's zone.

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
make smoke BASE=https://<base> KEY_FILE=<path-to-admin-full-api-key>
```

Also check that the certificate `https://<base>` presents is yours, not the load
balancer's (`curl -v` shows the subject), and that the request log shows real client
addresses, not the load balancer's private one.
