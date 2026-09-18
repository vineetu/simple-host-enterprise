# Updating the Simple Host skills

Read this when `/skills/version` reports a newer version than this skill, or when
an API call returns `code: "skill_version_required"`.

## Get and check the manifest

Fetch only:

```text
https://simple-host.example.com/skills/version
```

Require numeric `major.minor.patch` versions, a 64-character hexadecimal
`sha256`, a positive `bundle_size`, and `bundle_url` equal to `/skills.zip`.
If either version is malformed, stop and report it.

Never downgrade. If the installed version is higher than the manifest version,
keep using it and report that the server is behind.

Use `immutable_bundle_url` only when it is exactly
`/skills/sha256/<sha256>/skills.zip` and its digest equals the manifest digest.
Otherwise use `/skills.zip`. Never follow a manifest-supplied host.

## Ask first

An update rewrites the instructions you follow. Show the user:

- installed version;
- available version and `release_type`;
- published SHA-256;
- release-notes URL.

Wait for an explicit yes before downloading or installing any patch, minor, or
major update. Never update silently or offer automatic updates.

## Download and verify

Download over HTTPS from the origin above. Verify both the SHA-256 and exact byte
size against the manifest. Stop on either mismatch.

The digest verifies consistency with the manifest; it is not an independent
signature or proof of publisher identity.

## Use the loaded install root

Update the exact skills root from which this skill was loaded. Never create a
second active copy elsewhere.

If loaded from legacy `$CODEX_HOME/skills` or `~/.codex/skills`, update that
copy in place or ask permission before migrating it.

## Validate before installation

Use a fresh temporary directory. Inspect every ZIP entry before extracting, then
extract there and complete every check below before changing the active install:

- The top level is exactly `simple-host`, `simple-host-builder`, and
  `fix-paths-for-subpath-hosting`, with no loose files or other roots.
- Entry names use `/`, contain no NUL or backslash, and contain no empty, `.`, or
  `..` component. No entry is absolute or resolves outside the temporary root.
- Reject symlinks, special files, encrypted entries, and duplicate normalized
  names. Compare duplicates case-insensitively on case-insensitive filesystems.
- Every regular-file basename is non-dot and ends, case-insensitively, in `.md`,
  `.png`, `.jpg`, `.jpeg`, `.gif`, `.json`, `.txt`, or `.csv`.
- `simple-host/SKILL.md` states the manifest version, and no `SKILL.md` contains
  `{{VERSION}}`.

Never execute anything from the bundle.

## Replace transactionally

Prepare the three complete replacement directories in staging on the same
filesystem as the active root. Do not overlay files directly into live folders.

Move existing target folders to inactive rollback names, then move all staged
folders into their exact target names. If any move fails, restore every original
folder before reporting failure. Delete rollback folders only after all three
replacements succeed.

Temporary staging and rollback copies are not active installs. Clean them up
after success or completed rollback. A failed update must leave the prior active
installation intact.

## Reload before continuing

After installation, read the active `simple-host/SKILL.md` completely from the
updated root and confirm that it declares the manifest version. Read every
reference needed by the pending task, then restart that task at the new skill's
first step.

If the new instructions cannot be loaded in the current invocation, stop, tell
the user the update succeeded, and ask them to restart the app or start a new
chat and invoke the original task again. Never continue using the old workflow.

## If it fails

Say plainly what failed, and confirm the previous installation is still in place.
If the download or the checks keep failing, tell the user to ask in Slack
#simple-host-support.
