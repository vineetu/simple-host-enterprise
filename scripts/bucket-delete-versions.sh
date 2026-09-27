#!/usr/bin/env bash
# Delete every object version and delete marker under a prefix of an
# S3-compatible bucket, so the bucket can be deleted (docs/uninstall.md,
# step 5). Works on any provider that speaks the S3 API: it signs with
# curl's --aws-sigv4, lists ?versions and deletes each ?versionId.
#
#   BUCKET_URL=https://<endpoint>/<bucket> REGION=<region> CRED_FILE=<file> PREFIX=<prefix> ./scripts/bucket-delete-versions.sh
#
# CRED_FILE is a 0600 curl config file with one line, user = "<key-id>:<secret>",
# so the secret is never on a command line. PREFIX empty means the whole
# bucket. DRY_RUN=1 prints what it would delete. This cannot be undone.
set -eu

BUCKET_URL="${BUCKET_URL%/}"
REGION="${REGION:-}"
CRED_FILE="${CRED_FILE:-}"
PREFIX="${PREFIX:-}"
DRY_RUN="${DRY_RUN:-}"
die() { echo "bucket-delete-versions: $*" >&2; exit 2; }
case "$BUCKET_URL" in https://?*/?*) ;; *) die "set BUCKET_URL=https://<endpoint>/<bucket>" ;; esac
[ -n "$REGION" ] || die "set REGION to the bucket's region"
[ -f "$CRED_FILE" ] && [ -r "$CRED_FILE" ] || die "set CRED_FILE to a readable curl config file holding user = \"<key-id>:<secret>\""
for tool in curl python3; do command -v "$tool" >/dev/null || die "missing: $tool"; done

work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT
s3() { curl -fsS --max-time 60 -K "$CRED_FILE" --aws-sigv4 "aws:amz:$REGION:s3" "$@"; }
enc() { python3 -c 'import sys, urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$1"; }

total=0
while :; do
  s3 -o "$work/list.xml" "$BUCKET_URL?versions&max-keys=1000&prefix=$(enc "$PREFIX")"
  # One "key<TAB>versionId" line per version and delete marker, key URL-encoded.
  python3 - "$work/list.xml" > "$work/list.txt" <<'PY'
import sys, urllib.parse, xml.etree.ElementTree as ET
root = ET.parse(sys.argv[1]).getroot()
ns = root.tag.split("}")[0] + "}" if root.tag.startswith("{") else ""
for kind in ("Version", "DeleteMarker"):
    for el in root.iter(ns + kind):
        key = el.findtext(ns + "Key")
        vid = el.findtext(ns + "VersionId") or "null"
        print(urllib.parse.quote(key, safe="/") + "\t" + urllib.parse.quote(vid, safe=""))
PY
  [ -s "$work/list.txt" ] || break
  while IFS="$(printf '\t')" read -r key vid; do
    if [ -n "$DRY_RUN" ]; then
      echo "would delete $key version $vid"
    else
      s3 -o /dev/null -X DELETE "$BUCKET_URL/$key?versionId=$vid"
    fi
    total=$((total + 1))
  done < "$work/list.txt"
  [ -z "$DRY_RUN" ] || break
done
if [ -n "$DRY_RUN" ]; then
  echo "$total version(s) and delete marker(s) listed (first page only); nothing deleted"
else
  echo "deleted $total version(s) and delete marker(s)"
fi
