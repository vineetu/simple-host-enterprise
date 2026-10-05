# Your home page

Your person address opens your showcase by default. In the dashboard, choose one
of your personally owned sites as home. It opens at its existing site URL, with
its ordinary OIDC hand-off and access rules; its files and saved data keep that
site's origin. An inaccessible, unpublished, deleted, taken-down home or a
pending owner certificate leaves the normal index in place. Rename follows the
site ID; deletion and transfer clear the choice. Team addresses keep their index.

Custom homes fetch `/showcase.json` on their own site host. The same feed is
available on the person host, behind company sign-in, with `Cache-Control:
no-store` and no cross-origin credential sharing. It contains `owner`, `bio` and
`sites` (`name`, `url`, `updated_at`, `pinned`, `order`), with exactly the owner
index's visibility: the owner sees all published work, colleagues see only
listed work and never restricted sites. It never changes access or listing.

Save a plain-text bio, pin sites and set manual order in the dashboard or
through the signed-in connector: `set_home_page`, `set_bio`, `set_showcase_site`.
Pinned sites come first, then ascending order, then newest update and name.
The bio limit is `SHOWCASE_BIO_MAX_LENGTH` (280 characters by default; 1–2000).
Browser and connector REST use `GET`/`PUT /api/me/home` (`{"site":"portfolio"}`
or `{"site":null}`), `GET`/`PUT /api/me/bio` (`{"bio":"My projects"}`), and
`GET`/`PUT /api/sites/{sitename}/showcase` (`{"pinned":true,"order":10}`). These
account settings require OIDC identity; CI API keys cannot change them. All
changes are audited transactionally. Settings live in Postgres; site content
stays in the bucket and pods remain stateless. No custom domains or short names.
