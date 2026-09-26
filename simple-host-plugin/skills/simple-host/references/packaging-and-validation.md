# Packaging, validation, upload, and verification

Read this file completely before every upload. Validate the final static output
directory, not the project root, unless the project is genuinely raw HTML with no
build system.

## Relative paths in, quoted address out

Build with relative asset paths (`./`), as `frameworks.md` describes, so the
site works on its owner's host and on its own host if it is later restricted.
Report the `url` and `public_path` the API returned, exactly as returned, and
never an address you assembled.

## Preflight: mechanical limits

- Reject an empty directory.
- Require `index.html` at the archive root unless the user has explicitly
  identified a different final static entrypoint supported by the site.
- Reject a packaged archive larger than 100 MiB.
- Reject more than 50,000 archive entries, more than 500 MiB of aggregate
  uncompressed regular-file content, or a regular file larger than 500 MiB.
- Reject symlinks, devices, sockets, and other special files.
- Reject paths deeper than 32 components, paths longer than 1,024 bytes, or path
  components longer than 255 bytes. Reject absolute, traversal,
  control-character, invalid-UTF-8, or non-canonical paths.
- Warn on any individual file over 25 MiB as a deployment-practicality concern;
  this is not the server's hard per-file limit.
- Warn if `node_modules/` is present; this usually means the source tree was
  selected instead of build output.
- Reject `.env` files and other secret-bearing local configuration.
- Reject the case-insensitive extension denylist: source scripts `.sh`,
  `.bash`, `.zsh`, `.fish`, `.bat`, `.cmd`, `.ps1`, `.py`, `.pyc`, `.rb`,
  `.pl`, `.go`, `.php`; Windows executables and script hosts `.exe`, `.dll`,
  `.msi`, `.msix`, `.appx`, `.scr`, `.com`, `.pif`, `.cpl`, `.hta`, `.vbs`,
  `.vbe`, `.jse`, `.wsf`, `.wsh`, `.lnk`, `.reg`; other packages and disk
  images `.jar`, `.apk`, `.aab`, `.pkg`, `.deb`, `.rpm`, `.iso`, `.img`. This
  is not an allowlist: `.js`, and regular ZIP and DMG downloads, are allowed.
- Exclude `.DS_Store` and AppleDouble `._*` metadata entries.
- Exclude the project's `simple-host.json` marker. For a raw HTML site the
  packaging root is the published directory, so the commands below would
  otherwise upload it.

## Preflight: semantic checks

- If `package.json` has a build script and the selected directory is the project
  root, stop and build the framework output first.
- Never upload React, Vue, Next.js, Svelte, Astro, Nuxt, Gatsby, or Angular source
  in place of its static output.
- Flag server entrypoints and functionality: Express, `server.js`, API routes,
  server actions that require a runtime, Python/Go/PHP servers, SSR adapters, and
  Nuxt server handlers cannot run on Simple Host.
- Flag absolute local filesystem references such as `/Users/...`, `C:\...`, and
  `file:///...`.
- Check filename/reference casing as it will run on a case-sensitive server.
- Keep deployed HTML, CSS, and JavaScript readable where the toolchain permits;
  do not add minification or obfuscation unless the user asks.

## Verify asset paths

For a framework build, inspect the generated `index.html`:

```bash
grep -o 'src="[^"]*"' <build-dir>/index.html
grep -o 'href="[^"]*"' <build-dir>/index.html
```

```powershell
Select-String -Path (Join-Path '<build-dir>' 'index.html') -Pattern 'src="[^"]*"', 'href="[^"]*"' -AllMatches
```

Every site-owned asset should be relative (`./assets/...` or `assets/...`). A
bare `/assets/`, `/_app/`, or `/chunks/` path means the framework was built for
the wrong base; rebuild it. The exception is a framework from the "absolute
base" table in `frameworks.md`, whose assets must begin with `/<sitename>/`.

For raw HTML processed by `fix-paths-for-subpath-hosting`, inspect its own source
files for remaining root-relative paths:

```bash
grep -rn '"/[a-zA-Z]' <dir>
grep -rn "'/[a-zA-Z]" <dir>
```

```powershell
Get-ChildItem -LiteralPath '<dir>' -Recurse -File |
    Select-String -Pattern '"/[a-zA-Z]', "'/[a-zA-Z]"
```

Review intentional external/root endpoints separately. If a site-owned asset is
wrong, fix the source/config and rebuild; do not rewrite compiled chunks.

## Package at the archive root

On macOS/Linux:

```sh
tar -czf /tmp/<sitename>.tar.gz --exclude=./simple-host.json -C <final-static-dir> .
```

On Windows PowerShell 5.1 or 7, ZIP the directory contents without a wrapper and
include hidden files:

```powershell
Add-Type -AssemblyName System.IO.Compression.FileSystem
$Source = (Resolve-Path -LiteralPath '<final-static-dir>').Path
$Zip = Join-Path ([IO.Path]::GetTempPath()) '<sitename>.zip'
if (Test-Path -LiteralPath $Zip) { Remove-Item -LiteralPath $Zip -Force }
[IO.Compression.ZipFile]::CreateFromDirectory(
    $Source,
    $Zip,
    [IO.Compression.CompressionLevel]::Optimal,
    $false
)
```

PowerShell's `CreateFromDirectory` has no exclude, so when the packaging root is
also the published directory, stage a copy without `simple-host.json` and zip
that.

Inspect the finished archive again for size, entry count, paths, special files,
denylisted extensions, and a root `index.html`.

## Upload a new site

Name the owner explicitly, for personal and team namespaces alike. The owner is
the namespace resolved before the build — never assumed to be the authenticated
user:

```http
POST /api/collaboration/sites/<owner>/<sitename>
X-API-Key: <api_key>
X-Skill-Version: <installed skill version>
Content-Type: application/gzip

<archive bytes>
```

For a Windows ZIP use `Content-Type: application/zip`. Example:

```powershell
$Headers = @{
    'X-API-Key' = '<api_key>'
    'X-Skill-Version' = '<installed skill version>'
}
Invoke-WebRequest -UseBasicParsing -Method Post `
    -Uri '{{BASE_URL}}/api/collaboration/sites/<owner>/<sitename>' `
    -Headers $Headers -ContentType 'application/zip' -InFile $Zip
```

Creating requires `access_role` `owner` or `member` in that namespace.

The owner-inferred `POST /api/sites/<sitename>` still exists for older clients.
Do not use it, and do not use owner-inferred `PUT /api/sites/<sitename>` at all.
Every existing site—personal or team-owned—must use the owner-qualified
collaboration route and retained `If-Match` described in `collaboration.md`.

## Who can open it

A new site opens only for its owner (for a team site, the team's members).
After a first publish, tell the user that, ask who should see it, and set the
level (connector: `set_site_access`):

```http
POST /api/collaboration/sites/<owner>/<sitename>/access
X-API-Key: <api_key>
X-Skill-Version: <installed skill version>
Content-Type: application/json

{"level":"company"}
```

The levels and the rules for `network` are in `collaboration.md` section 6.
`listed` makes the active HTML eligible for the company showcase and search
after asynchronous indexing; a lower level drops it from new search
immediately. Search returns at most one best active page per site.

## Handle responses

- `400 skill_version_required`: stop, update with permission, reload the skill,
  and restart at access resolution.
- `401`: the key is missing/invalid; follow account recovery rather than
  repeatedly registering.
- `403`: the actor lacks the required role in that namespace. With
  `code: "no_access"`, stop and report it; do not retry without the owner and do
  not create the site somewhere you can write.
- `409` on a create: the server refused the name. Relay its message and ask the
  human rather than guessing a variation.
- `412`: another actor changed the existing site; stop and reconcile.
- `428`: the update requires the collaboration ETag workflow.
- `429`: honor integer-seconds `Retry-After`; never hot-loop.

An upload success includes `active_version` and the site's `url`, but it is not
the end of verification.

## Post-upload verification

Open the `url` from the upload response, and report that exact value. Do not
compose it.

Confirm:

1. The entrypoint returns success and renders expected content.
2. JavaScript, CSS, fonts, images, chunks, and navigation load, with no asset
   404s.
3. The deployed site behavior matches the intended local build.

If asset paths 404, rebuild with relative paths. If source was uploaded, upload
the build output. If casing differs, fix it and rebuild.
