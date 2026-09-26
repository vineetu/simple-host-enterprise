# Security policy

## Reporting a vulnerability

Report it privately through GitHub: on this repository's **Security** tab,
choose **Report a vulnerability**. That opens a private security advisory
visible only to you and the maintainer (the repository owner). Please do not
open a public issue, pull request or discussion for a suspected vulnerability.

A useful report says which version or commit you tested, how the installation
was configured where it matters (identity provider, overlay), the steps to
reproduce, and what an attacker gains.

## Supported versions

Only the latest release is supported. Fixes are made on `main` and shipped in
a new release; older releases are not patched. Build from a release tag, or
pin the release image by digest, and upgrade to take a fix.

## Scope

In scope: the code in this repository and the release image built from it,
including the server, its management API and MCP endpoint, sign-in and
session handling, isolation between sites and between owners, archive and asset
handling, the schema and its migrations, and the manifests under `deploy/`.

Out of scope: vulnerabilities in your cluster, ingress controller, identity
provider, database or object storage; content that your own users publish
(pages are arbitrary HTML and JavaScript by design); and findings that depend
on a configuration `docs/configuration.md` tells you not to use, such as the
local-evaluation overrides `DB_INSECURE_ALLOWED` and
`BACKUP_STORAGE_INSECURE_ALLOWED`. `docs/security-review.md` records the
known limitations; a way around one of them is in scope.

## Maintenance

Simple Host is maintained by one person on a best-effort basis. There is no
response-time commitment, no support contract and no bug bounty. Reports are
read, and fixes are released as quickly as that allows.
