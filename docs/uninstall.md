# Uninstalling a real install

For the local overlay, `make local-down` is the whole of it. This page is
for an install on a real cluster (`deploy/overlays/byo`, `staging`,
`production`): what the install created, in the order to remove it, and
what to copy out first.

Work through it with whoever owns DNS, the identity provider, the
database and the bucket; most of what an install leaves behind is outside
the cluster.

## 1. Decide what to keep, and export it

Everything people made is in two places: the bucket (pages and uploaded
files) and Postgres (saved data and its history, access levels, viewers,
teams, the audit log and the access log). Decide with the owner of the
pilot what must outlive it, then:

- **Tell people, with a date.** Each owner or team member can download a
  site whole from the dashboard (**Download site**: live files, saved data
  and its history, versions and uploaded files as one zip), or ask their
  agent to (`export_site`).
- **The audit trail.** As an admin, download the audit log and the access
  log for the whole retention window from /admin (or
  `GET /api/admin/export?kind=audit&format=jsonl` and `kind=access`). Your
  SIEM already holds the audit stream if you forwarded it.
- **A person's data** (a leaver, a data-subject request): disable them on
  /admin, then **Export data** on their row.
- **A complete copy** (everything, restorable): take a final snapshot of
  the managed database (or `pg_dump` it), and copy the bucket prefix
  (`BACKUP_STORAGE_PREFIX`) with your cloud's copy tool. If the install
  used `BACKUP_ENVELOPE_KEY`, the bucket copy is unreadable without that
  key: keep the key with the copy, or do not bother keeping the copy.

Then stop changes: scale the server to zero so nothing is written while
you remove it.

```sh
kubectl -n simple-host scale deploy/simple-host --replicas=0
```

## 2. Remove the DNS records first

**Remove `<base>` and `*.<base>` before you delete anything in the
cluster.** Deleting the namespace releases the load balancer, and a cloud
hands a released address or hostname to the next customer who asks. A
wildcard record left pointing at it lets a stranger serve any page they
like under every name on your domain, with a certificate they can get for
it, in front of colleagues who were told those addresses are the company's.

- Delete the `<base>` and `*.<base>` records (and any `_acme-challenge`
  records you added by hand for DNS-01).
- If the base domain was registered only for this install, keep the
  registration until its records are gone everywhere (and, if you let it
  lapse, know that whoever registers it next gets every old link).
- If you asked for the base domain to be on the Public Suffix List, ask for
  its removal.

## 3. Delete what the install created in the cluster

```sh
kustomize build deploy/overlays/byo | kubectl delete --ignore-not-found -f -
kubectl delete namespace simple-host --ignore-not-found
```

(Use your own overlay's path.) This removes the server, its Service and
Ingress, the owner-hosts reconciler (`simple-host-owner-hosts`: its
Deployment, ServiceAccount, Role and RoleBinding), the prune CronJob, the
ConfigMap and Secrets the overlay generated, and, with the namespace, every
per-owner Ingress the reconciler made and the Certificates and TLS Secrets
cert-manager issued for them. Nothing the package installs is cluster-wide.

Then check what was installed alongside it:

- **The ClusterIssuer**, if it was created only for this install (the one
  named in `OWNER_CERT_ISSUER`, and the DNS-01 one in the Ingress's
  `cert-manager.io/cluster-issuer` annotation). First make sure nothing
  else uses it:

  ```sh
  kubectl get certificates -A -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,ISSUER:.spec.issuerRef.name
  ```

  then `kubectl delete clusterissuer <name>`, and delete the Secret it
  names in the `cert-manager` namespace (a CA issuer's CA key, an ACME
  account key, a DNS provider credential). A CA created for this install
  should be removed from company devices' trust stores too.
- **cert-manager and ingress-nginx.** `make install` adds them only when
  the cluster had none. Remove them only if nothing else on the cluster
  uses them.
- **Image pull secrets and mirrored images**, if you used a private
  registry (`INSTALL.md`, "If you use a private registry").

## 4. The identity provider

Delete the OIDC application (client) registered for the install, which
revokes its client secret, and any group or claim mapping made for its
admin claim. Everyone's sessions have already ended with the server; no
account in the identity provider needs to change.

## 5. The bucket

Only after the export decision in step 1:

- Delete every object under `BACKUP_STORAGE_PREFIX`, **including
  noncurrent versions**: versioning is on (it has to be), so a plain delete
  leaves every version recoverable and billed. Most providers do this with
  a lifecycle rule that expires current and noncurrent versions, or a
  "delete bucket and all versions" action in the console.
- Delete the bucket if it was created for this install.
- Remove the credential the server used: the access key pair (and the user
  or service account it belongs to), or the workload identity binding and
  role (`docs/cloud/<your cloud>.md`, sections 4 and 5).

## 6. The database

- Take the final snapshot your retention policy asks for (step 1).
- Delete the database, or the whole instance if it was created for this
  install. Point-in-time backups and automated snapshots often outlive the
  instance for their retention period; delete them too if the policy says
  so.
- Drop the `simplehost_app` role and the owner role if the instance stays.

## 7. Secrets kept anywhere else

Delete the copies kept outside the cluster, once nothing in step 1 still
needs them:

- `deploy/overlays/<overlay>/secrets.env` and `db-ca.crt` on whoever's
  machine ran the install, and any secret manager entries they came from:
  the OIDC client secret, the session signing keys, the database owner and
  app passwords, the bucket keys, SMTP credentials.
- The escrowed `BACKUP_ENVELOPE_KEY`, **only** once no copy of the bucket
  that you mean to read is left.
- Your monitoring's scrape configuration for the metrics port, alert rules,
  and the log shipper route that forwarded the audit stream.

## 8. Check

- `dig <base>` and `dig anything.<base>` return nothing.
- `kubectl get ns simple-host` is gone, and
  `kubectl get clusterissuer,ingressclass` lists only what other workloads
  use.
- The bucket and the database (with its backups) are gone or deliberately
  kept, and the OIDC client no longer exists.
