#!/usr/bin/env bash
# Smoke test against a real installation, over public HTTPS only. No
# kubectl, no database, no test identity provider: everything goes through
# the same endpoints an agent uses, authenticated by one API key an admin
# minted on /dashboard.
#
#   make smoke BASE=https://sites.example.com KEY_FILE=~/.simple-host-install-key
#   BASE=sites.example.com SIMPLE_HOST_API_KEY=... ./scripts/smoke-remote.sh
#
# The key comes from KEY_FILE, else SIMPLE_HOST_API_KEY, else a hidden prompt
# on a terminal. It is never printed and never put on a command line: curl
# reads the header from a 0600 file in a private temp directory.
#
# It publishes a throwaway site named smoke-<8 hex digits> in the key owner's
# namespace, checks that the old <owner>.<base>/<site>/ address serves it
# until the owner's wildcard certificate *.<owner>.<base> is ready (waiting
# up to CERT_WAIT seconds, default 300, for it), then that the site is served
# on its own host <site>.<owner>.<base> and the old address and the v1.2
# <owner>--<site>.<base> host redirect there, updates it,
# rolls it back, reads and writes its state, uploads, lists and deletes an
# asset, restricts it and checks it stays on its own host, then deletes it. The site is deleted on every exit path,
# including a failure or Ctrl-C.
#
# Optional: OTHER_KEY_FILE, a key belonging to a different, non-admin person
# who is not a viewer of the throwaway site. With it, the run also checks
# that a signed-in stranger gets 404 from the restricted site's host.
#
# Exit status is the number of failures, capped at 125.
set -u

BASE="${BASE:-}"
SKILL_VERSION="${SKILL_VERSION:-0.13.1}"
CERT_WAIT="${CERT_WAIT:-300}"

die() { echo "smoke-remote: $*" >&2; exit 2; }

# BASE may be given as a hostname or as an origin; nothing else.
BASE="${BASE#https://}"
BASE="${BASE%/}"
case "$BASE" in
  "") die "set BASE to the install's base host, e.g. BASE=https://sites.example.com" ;;
  http://*) die "BASE must be https (got http://)" ;;
  */*|*" "*|*:*) die "BASE must be a bare host or https://host, nothing after it (got '$BASE')" ;;
esac

for tool in curl tar python3; do
  command -v "$tool" >/dev/null || die "missing: $tool"
done

work="$(mktemp -d)" || die "cannot create a temp directory"
chmod 700 "$work"
site=""
owner=""
# shellcheck disable=SC2329  # called by the EXIT trap
cleanup() {
  if [ -n "$site" ] && [ -n "$owner" ] && [ -f "$work/key.hdr" ]; then
    curl -sS -o /dev/null -X DELETE -H @"$work/key.hdr" -H "X-Skill-Version: $SKILL_VERSION" \
      "https://$BASE/api/collaboration/sites/$owner/$site" 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# read_key <file-or-empty> <env-value-or-empty> <prompt-or-empty>
read_key() {
  local file="$1" env="$2" prompt="$3" v=""
  if [ -n "$file" ]; then
    v="$(tr -d '\r\n' < "$file")"
  elif [ -n "$env" ]; then
    v="$env"
  elif [ -n "$prompt" ] && [ -t 0 ]; then
    printf '%s' "$prompt" >&2
    IFS= read -rs v; echo >&2
  fi
  v="${v#"${v%%[![:space:]]*}"}"; v="${v%"${v##*[![:space:]]}"}"
  printf '%s' "$v"
}

for var in KEY_FILE OTHER_KEY_FILE; do
  f="${!var:-}"
  [ -n "$f" ] || continue
  f="${f/#\~/$HOME}"
  [ -f "$f" ] && [ -r "$f" ] || die "cannot read $var ($f)"
  printf -v "$var" '%s' "$f"
done

key="$(read_key "${KEY_FILE:-}" "${SIMPLE_HOST_API_KEY:-}" "Simple Host API key (hidden): ")"
[ -n "$key" ] || die "no API key: set KEY_FILE=<path>, or SIMPLE_HOST_API_KEY, or run on a terminal to be prompted"
case "$key" in *[[:space:]]*) die "the API key contains whitespace; check the key file" ;; esac
( umask 077; printf 'X-API-Key: %s\n' "$key" > "$work/key.hdr" )
unset key SIMPLE_HOST_API_KEY

other=""
if [ -n "${OTHER_KEY_FILE:-}" ]; then
  other="$(read_key "$OTHER_KEY_FILE" "" "")"
  [ -n "$other" ] || die "OTHER_KEY_FILE is empty"
  ( umask 077; printf 'X-API-Key: %s\n' "$other" > "$work/other.hdr" )
  unset other
  other=1
fi

fails=0
pass=0
K=(-H @"$work/key.hdr" -H "X-Skill-Version: $SKILL_VERSION")

ok()   { pass=$((pass+1)); printf 'ok    %s\n' "$1"; }
bad()  { fails=$((fails+1)); printf 'FAIL  %s\n' "$1"; [ -s "$work/body" ] && { head -c 300 "$work/body"; echo; }; }

# req <host> <path> [curl args...] — sets $code and leaves the body in $work/body.
req() {
  local host="$1" path="$2"; shift 2
  : > "$work/body"
  code="$(curl -sS --max-time 60 -o "$work/body" -w '%{http_code}' "$@" "https://$host$path" 2>"$work/err")" || code="ERR"
  if [ "$code" = "ERR" ] || [ "$code" = "000" ]; then
    code="ERR"; head -c 300 "$work/err" > "$work/body"
  fi
}

# expect <want> <label> <host> <path> [curl args...]; want "2xx" matches any success.
expect() {
  local want="$1" label="$2"; shift 2
  req "$@"
  if [ "$code" = "$want" ] || { [ "$want" = 2xx ] && [[ "$code" == 2[0-9][0-9] ]]; }; then
    ok "$label ($code)"
  else
    bad "$label: got $code (want $want)"
  fi
}

# json <python expression over d> — evaluated against the last body.
json() {
  python3 -c 'import json,sys
d = json.load(open(sys.argv[1]))
print(eval(sys.argv[2]))' "$work/body" "$1" 2>/dev/null
}

host_of() {
  python3 -c 'import sys, urllib.parse; print(urllib.parse.urlsplit(sys.argv[1]).hostname or "")' "$1" 2>/dev/null
}

owner_host=""
site_host=""
finish() {
  echo
  if [ -n "$owner_host" ] && [ "$fails" -eq 0 ]; then
    echo "Browser check, by hand: $owner as themselves opens https://$owner_host/"
    echo "and, after a few redirects (the session hand-off), sees their own index."
    echo
  fi
  echo "passed $pass, failed $fails"
  [ "$fails" -gt 125 ] && fails=125
  exit "$fails"
}

echo "== $BASE: probes and TLS (a certificate error fails here)"
expect 200 "readyz" "$BASE" /readyz
if [ "$code" != 200 ]; then echo "the instance is not ready; stopping"; finish; fi
expect 200 "healthz" "$BASE" /healthz

echo "== API key"
expect 401 "no credentials are refused" "$BASE" /api/sites
expect 401 "a wrong key is refused" "$BASE" /api/sites -H "X-API-Key: smoke-not-a-key" -H "X-Skill-Version: $SKILL_VERSION"
expect 200 "the key authenticates" "$BASE" /api/me "${K[@]}"
if [ "$code" != 200 ]; then echo "the key does not authenticate; stopping (was it revoked?)"; finish; fi
owner="$(json 'd["username"]')"
[ -n "$owner" ] || { bad "GET /api/me carried no username"; finish; }
ok "key belongs to $owner (admin: $(json 'd.get("is_admin")'))"

site="smoke-$(od -An -N4 -tx1 /dev/urandom | tr -d " \n")"
echo "== publish $owner/$site"
mkdir -p "$work/v1" "$work/v2"
printf '<!doctype html><title>%s</title><h1>smoke v1</h1>\n' "$site" > "$work/v1/index.html"
printf '<!doctype html><title>%s</title><h1>smoke v2</h1>\n' "$site" > "$work/v2/index.html"
tar -czf "$work/v1.tar.gz" -C "$work/v1" .
tar -czf "$work/v2.tar.gz" -C "$work/v2" .
expect 201 "publish" "$BASE" "/api/collaboration/sites/$owner/$site" -X POST "${K[@]}" \
  -H "Content-Type: application/gzip" --data-binary @"$work/v1.tar.gz"
if [ "$code" != 201 ]; then site=""; echo "publish failed; stopping"; finish; fi
site_url="$(json 'd.get("url","")')"
owner_label="$(printf '%s' "$owner" | tr '[:upper:]' '[:lower:]' | tr . -)"
site_host="$site.$owner_label.$BASE"
owner_host="$owner_label.$BASE"
case "$site_url" in
  "https://$site_host/"|"https://$owner_host/$site/") ok "site address is $site_url" ;;
  *) bad "publish returned '$site_url' (want https://$site_host/, or https://$owner_host/$site/ while the certificate is pending)"; finish ;;
esac

# Update and rollback require If-Match with the site's current ETag.
expect 200 "read the site" "$BASE" "/api/collaboration/sites/$owner/$site" "${K[@]}"
etag="$(json 'd["etag"]')"
expect 428 "an update without If-Match is refused" "$BASE" "/api/collaboration/sites/$owner/$site" -X PUT "${K[@]}" \
  -H "Content-Type: application/gzip" --data-binary @"$work/v2.tar.gz"
expect 2xx "update to version 2" "$BASE" "/api/collaboration/sites/$owner/$site" -X PUT "${K[@]}" \
  -H "If-Match: $etag" -H "Content-Type: application/gzip" --data-binary @"$work/v2.tar.gz"
expect 200 "read the site after the update" "$BASE" "/api/collaboration/sites/$owner/$site" "${K[@]}"
active="$(json 'd["active_version"]')"; etag="$(json 'd["etag"]')"
if [ "$active" = 2 ]; then ok "active version is 2 after the update"; else bad "active version is '$active' after the update (want 2)"; fi
expect 2xx "roll back to version 1" "$BASE" "/api/collaboration/sites/$owner/$site/rollback" -X POST "${K[@]}" \
  -H "If-Match: $etag" -H "Content-Type: application/json" -d '{"version":1}'
expect 200 "read the site after the rollback" "$BASE" "/api/collaboration/sites/$owner/$site" "${K[@]}"
active="$(json 'd["active_version"]')"
if [ "$active" = 1 ]; then ok "active version is 1 after the rollback"; else bad "active version is '$active' after the rollback (want 1)"; fi

echo "== the owner host $owner_host and the fallback"
expect 401 "the owner index needs a signed-in session" "$owner_host" /
expect 404 "the owner host serves no API" "$owner_host" /api/me
if curl -sS -o /dev/null --max-time 20 "https://$site_host/healthz" 2>/dev/null; then
  echo "  (*.$owner_host already has its certificate; the fallback cannot be observed on this run)"
else
  expect 401 "until the certificate is ready the old address serves the site (needs a session, no redirect)" "$owner_host" "/$site/"
fi

# Each server replica re-reads which owners are ready every 15 s; the 20 s
# pause after the first redirect lets every replica agree.
echo "== waiting up to ${CERT_WAIT}s for the certificate of *.$owner_host and the switch to the redirect"
ready=""
for _ in $(seq 1 $((CERT_WAIT / 5 + 1))); do
  if curl -sS -o /dev/null --max-time 20 "https://$site_host/healthz" 2>/dev/null && [ "$(curl -sS -o /dev/null --max-time 20 -w '%{http_code}' "https://$owner_host/$site/" 2>/dev/null)" = "301" ]; then ready=1; sleep 20; break; fi
  sleep 5
done
if [ -n "$ready" ]; then ok "$site_host presents a valid certificate and the old address redirects"; else bad "$site_host not ready after ${CERT_WAIT}s"; finish; fi

echo "== the site host $site_host"
expect 401 "hosted content needs a signed-in session" "$site_host" /
expect 301 "the old address redirects to the site host" "$owner_host" "/$site/a.html?q=1" -D "$work/hdr"
loc="$(tr -d '\r' < "$work/hdr" | sed -n 's/^[Ll]ocation: //p' | tail -1)"
if [ "$loc" = "https://$site_host/a.html?q=1" ]; then ok "the redirect keeps path and query"; else bad "the redirect went to '$loc' (want https://$site_host/a.html?q=1)"; fi
expect 308 "an old state write redirects keeping its method" "$owner_host" "/api/sites/$site/state/versioned" -X PUT "${K[@]}" -H "Content-Type: application/json" -d '{}'
expect 301 "the v1.2 dash host redirects to the site host" "$owner_label--$site.$BASE" "/p?q=1" -D "$work/hdr"
loc="$(tr -d '\r' < "$work/hdr" | sed -n 's/^[Ll]ocation: //p' | tail -1)"
if [ "$loc" = "https://$site_host/p?q=1" ]; then ok "the dash host redirect keeps path and query"; else bad "the dash host redirected to '$loc' (want https://$site_host/p?q=1)"; fi

echo "== state"
expect 200 "state read" "$site_host" "/api/sites/$site/state/versioned" "${K[@]}"
ver="$(json 'd["version"]')"; ver="${ver:-0}"
expect 200 "state write" "$site_host" "/api/sites/$site/state/versioned" -X PUT "${K[@]}" \
  -H "Content-Type: application/json" -d "{\"version\":$ver,\"state\":{\"smoke\":\"$site\"}}"
expect 200 "state read after write" "$site_host" "/api/sites/$site/state/versioned" "${K[@]}"
if [ "$(json 'd["state"].get("smoke","")')" = "$site" ]; then ok "state round-trips"; else bad "state read did not return the write"; fi
expect 409 "a stale version is refused" "$site_host" "/api/sites/$site/state/versioned" -X PUT "${K[@]}" \
  -H "Content-Type: application/json" -d "{\"version\":$ver,\"state\":{}}"

echo "== assets"
printf 'smoke asset\n' > "$work/notes.txt"
expect 2xx "asset upload" "$site_host" "/api/sites/$site/assets" -X POST "${K[@]}" \
  -F "file=@$work/notes.txt;filename=notes.txt;type=text/plain"
asset_id="$(json 'd["id"]')"
if [ -n "$asset_id" ]; then
  ok "asset id returned"
  expect 200 "asset list" "$site_host" "/api/sites/$site/assets" "${K[@]}"
  if grep -q '"notes.txt"' "$work/body"; then ok "asset appears in the list"; else bad "asset missing from the list"; fi
  expect 2xx "asset delete" "$site_host" "/api/sites/$site/assets/$asset_id" -X DELETE "${K[@]}"
else
  bad "asset upload returned no id"
fi

echo "== restrict the site to its owner"
expect 2xx "add the owner as its only viewer" "$BASE" "/api/collaboration/sites/$owner/$site/viewers" -X POST "${K[@]}" \
  -H "Content-Type: application/json" -d "{\"usernames\":[\"$owner\"]}"
expect 200 "read the site back" "$BASE" "/api/collaboration/sites/$owner/$site" "${K[@]}"
restricted_host="$(host_of "$(json 'd.get("url","")')")"
if [ "$restricted_host" = "$site_host" ]; then ok "the restricted site stays on $site_host"; else bad "the restricted site moved to '$restricted_host' (want $site_host)"; fi
expect 301 "the old address still redirects, confirming nothing" "$owner_host" "/$site/"
if [ -n "$restricted_host" ]; then
  expect 401 "the restricted site needs a signed-in session" "$restricted_host" /
  expect 401 "the restricted site refuses a wrong key" "$restricted_host" /api/site/state/versioned -H "X-API-Key: smoke-not-a-key"
  expect 200 "the owner's key still reads its state" "$restricted_host" /api/site/state/versioned "${K[@]}"
  if [ -n "$other" ]; then
    expect 404 "a signed-in stranger gets 404" "$restricted_host" /api/site/state/versioned \
      -H @"$work/other.hdr" -H "X-Skill-Version: $SKILL_VERSION"
  else
    echo "  (a signed-in stranger's 404 is checked only with OTHER_KEY_FILE)"
  fi
fi

echo "== clean up"
deleted="$site"
expect 2xx "delete the throwaway site" "$BASE" "/api/collaboration/sites/$owner/$deleted" -X DELETE "${K[@]}"
[[ "$code" == 2[0-9][0-9] ]] && site=""
expect 404 "it is gone" "$BASE" "/api/collaboration/sites/$owner/$deleted" "${K[@]}"
finish
