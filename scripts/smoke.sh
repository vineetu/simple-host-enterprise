#!/usr/bin/env bash
# Smoke test against a running installation. Signs in through Dex as the
# local overlay's two static test accounts, mints and revokes an API key,
# publishes a one-page site, and checks that the base host, the owner host
# (index, the fallback that serves a site until the owner's wildcard
# certificate is ready, and the redirects after), the site's own host
# (<site>.<owner>.<base>, where every site is served), the probes and the
# request log behave. CERT_WAIT (seconds, default 300) bounds the wait for
# an owner's certificate. Exit status is the number of failures, so it can gate
# a rollout.
#
#   BASE=simple-host.127-0-0-1.nip.io CLUSTER_CONTEXT=docker-desktop NAMESPACE=simple-host ./scripts/smoke.sh
#
# Identity is OIDC sign-in now, not a pasted admin key, so
# this script drives the whole Authorization Code + PKCE dance against Dex
# with curl and a cookie jar. Dex's issuer is the in-cluster Service DNS
# name (deploy/components/dex/configmap.yaml) because every call it drives
# from the *server* (discovery, token exchange, JWKS) is pod-to-Dex; the one
# step that is genuinely browser-shaped — loading the login form and
# posting credentials — reaches the same Service through a `kubectl
# port-forward` this script starts itself, using
# `curl --resolve <dex-host>:127.0.0.1` so the Host header Dex sees still
# matches its own issuer.
#
# The two accounts are fixed and local-only: admin@example.com / adminpass
# (also named in ADMIN_EMAILS) and person@example.com / personpass.

set -u
BASE="${BASE:?set BASE to the base hostname}"
CLUSTER_CONTEXT="${CLUSTER_CONTEXT:-docker-desktop}"
NAMESPACE="${NAMESPACE:-simple-host}"
SKILL_VERSION="${SKILL_VERSION:-0.13.1}"
CERT_WAIT="${CERT_WAIT:-300}"
DEX_HOST="dex.simple-host.svc.cluster.local"
DEX_PORT="5556"

fails=0
pass=0
work="$(mktemp -d)"
pf_pid=""
cleanup() {
  [ -n "$pf_pid" ] && kill "$pf_pid" >/dev/null 2>&1
  rm -rf "$work"
}
trap cleanup EXIT
code_fmt='%{http_code}'

# expect <status> <host> <path> [curl args...]
expect() {
  local want="$1" host="$2" path="$3"; shift 3
  local got
  curl -sS -o "$work/body" -w "$code_fmt" "$@" "https://$host$path" > "$work/code" 2>/dev/null || echo ERR > "$work/code"
  got="$(cat "$work/code")"
  if [ "$got" = "$want" ]; then
    pass=$((pass+1)); printf 'ok    %-4s %-45s %s\n' "$got" "$host" "$path"
  else
    fails=$((fails+1)); printf 'FAIL  %-4s %-45s %s (want %s)\n' "$got" "$host" "$path" "$want"
    head -c 300 "$work/body"; echo
  fi
}

# expect_header <host> <path> <header-name> [curl args...]
expect_header() {
  local host="$1" path="$2" header="$3"; shift 3
  if curl -sS -o /dev/null -D - "$@" "https://$host$path" 2>/dev/null | grep -qi "^$header:"; then
    pass=$((pass+1)); printf 'ok    hdr  %-45s %s carries %s\n' "$host" "$path" "$header"
  else
    fails=$((fails+1)); printf 'FAIL  hdr  %-45s %s lacks %s\n' "$host" "$path" "$header"
  fi
}

# expect_header_value <host> <path> <header-name> <want-value> [curl args...]
expect_header_value() {
  local host="$1" path="$2" header="$3" want="$4"; shift 4
  local got
  got="$(curl -sS -o /dev/null -D - "$@" "https://$host$path" 2>/dev/null | grep -i "^$header:" | tail -1 | sed "s/^[^:]*: *//" | tr -d '\r')"
  if [ "$got" = "$want" ]; then
    pass=$((pass+1)); printf 'ok    hdr  %-45s %s %s: %s\n' "$host" "$path" "$header" "$got"
  else
    fails=$((fails+1)); printf 'FAIL  hdr  %-45s %s %s = "%s", want "%s"\n' "$host" "$path" "$header" "$got" "$want"
  fi
}

# db_query <sql> — runs one statement inside postgres-0 as the application
# role, for the last_seen_at check below. There is no other way to observe
# a background side effect (a database column) from this script.
db_query() {
  local sql="$1" app_password
  app_password="$(kubectl --context "$CLUSTER_CONTEXT" -n "$NAMESPACE" get secret simple-host-secrets -o jsonpath='{.data.DB_APP_PASSWORD}' 2>/dev/null | base64 -d)"
  kubectl --context "$CLUSTER_CONTEXT" -n "$NAMESPACE" exec postgres-0 -- env PGPASSWORD="$app_password" \
    psql -U simplehost_app -d simplehost -h localhost -t -A -c "$sql" 2>/dev/null
}

# recent_pod_logs: the simple-host container's own logs since this run
# started, from every replica (a request may have hit any of them), used by a
# couple of checks below that have no database-visible side effect of their own.
recent_pod_logs() {
  kubectl --context "$CLUSTER_CONTEXT" -n "$NAMESPACE" logs -l app=simple-host -c simple-host --since=5m 2>/dev/null
}

check() {
  # check <description> <condition-command...>
  local desc="$1"; shift
  if "$@"; then
    pass=$((pass+1)); printf 'ok    %s\n' "$desc"
  else
    fails=$((fails+1)); printf 'FAIL  %s\n' "$desc"
  fi
}

# A rollout reports complete a moment before the ingress has the new
# endpoint; wait for the first honest answer rather than counting it a failure.
for _ in $(seq 1 30); do
  if [ "$(curl -sS -o /dev/null -w "$code_fmt" "https://$BASE/readyz" 2>/dev/null)" = "200" ]; then break; fi
  sleep 2
done

echo "== probes and control plane on $BASE"
expect 200 "$BASE" /healthz
expect 200 "$BASE" /readyz
expect 200 "$BASE" /
expect 200 "$BASE" /docs.html
expect_header "$BASE" / X-Request-Id
expect 401 "$BASE" /api/sites

# dex_sign_in <email> <password> <cookiejar>
#
# Walks GET /auth/login -> Dex's authorize endpoint -> Dex's local-connector
# login redirect -> the rendered login form -> POST the credentials -> our
# own GET /auth/callback, entirely with curl, leaving a session cookie in
# <cookiejar>. Fails loudly (and returns non-zero) at whichever hop breaks,
# so a protocol regression is diagnosable instead of a bare "sign-in failed."
dex_sign_in() {
  local email="$1" password="$2" jar="$3"
  local hdr1 hdr2 hdr3 hdr4 authorize_url login_action login_url callback_url

  hdr1="$work/hdr-login"; rm -f "$jar" "$hdr1"
  curl -sS -c "$jar" -D "$hdr1" -o /dev/null "https://$BASE/auth/login"
  authorize_url="$(grep -i '^location:' "$hdr1" | tail -1 | sed 's/^[Ll]ocation: //' | tr -d '\r')"
  if [ -z "$authorize_url" ]; then echo "  dex_sign_in($email): /auth/login gave no redirect"; return 1; fi

  hdr2="$work/hdr-authorize"
  curl -sS -b "$jar" -c "$jar" -D "$hdr2" -o "$work/loginpage.html" -L \
    --resolve "$DEX_HOST:$DEX_PORT:127.0.0.1" "$authorize_url"
  login_action="$(grep -o 'action="[^"]*"' "$work/loginpage.html" | head -1 | sed 's/action="//;s/"$//;s/\&amp;/\&/g')"
  if [ -z "$login_action" ]; then echo "  dex_sign_in($email): Dex login form not found"; return 1; fi
  login_url="http://$DEX_HOST:$DEX_PORT${login_action}"

  hdr3="$work/hdr-dexlogin"
  curl -sS -b "$jar" -c "$jar" -D "$hdr3" -o /dev/null \
    --resolve "$DEX_HOST:$DEX_PORT:127.0.0.1" \
    -X POST "$login_url" --data-urlencode "login=$email" --data-urlencode "password=$password"
  callback_url="$(grep -i '^location:' "$hdr3" | tail -1 | sed 's/^[Ll]ocation: //' | tr -d '\r')"
  if [ -z "$callback_url" ]; then
    echo "  dex_sign_in($email): Dex refused the credentials"; return 1
  fi

  hdr4="$work/hdr-callback"
  curl -sS -b "$jar" -c "$jar" -D "$hdr4" -o "$work/callback-body.html" \
    --resolve "$DEX_HOST:$DEX_PORT:127.0.0.1" "$callback_url"
  if ! grep -qi '^set-cookie: __Host-sh_session=' "$hdr4"; then
    echo "  dex_sign_in($email): callback did not set a session cookie:"
    head -c 300 "$work/callback-body.html"; echo
    return 1
  fi
  return 0
}

echo "== starting a port-forward to Dex ($DEX_HOST -> 127.0.0.1:$DEX_PORT)"
kubectl --context "$CLUSTER_CONTEXT" -n "$NAMESPACE" port-forward svc/dex "$DEX_PORT:$DEX_PORT" >"$work/port-forward.log" 2>&1 &
pf_pid=$!
for _ in $(seq 1 20); do
  grep -q "Forwarding from 127.0.0.1" "$work/port-forward.log" 2>/dev/null && break
  sleep 0.5
done

echo "== sign in as the admin test account"
admin_jar="$work/cj-admin"
if check "admin sign-in completes (admin@example.com)" dex_sign_in admin@example.com adminpass "$admin_jar"; then
  admin_me="$(curl -sS -b "$admin_jar" "https://$BASE/api/me")"
  owner="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["username"])' "$admin_me" 2>/dev/null || true)"
  check "admin is_admin is true" bash -c "echo '$admin_me' | grep -q '\"is_admin\":true'"
else
  owner=""
fi

echo "== sign in as the non-admin test account"
person_jar="$work/cj-person"
person_username=""
if check "non-admin sign-in completes (person@example.com)" dex_sign_in person@example.com personpass "$person_jar"; then
  person_me="$(curl -sS -b "$person_jar" "https://$BASE/api/me")"
  check "non-admin is_admin is false" bash -c "echo '$person_me' | grep -q '\"is_admin\":false'"
  person_username="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["username"])' "$person_me" 2>/dev/null || true)"
fi

if [ -z "$owner" ]; then
  echo "FAIL  could not resolve the admin account's username; stopping"
  echo "passed $pass, failed $((fails+1))"; exit $((fails+1))
fi

echo "== API keys: mint, use, list, revoke"
mint_body="$(curl -sS -b "$admin_jar" -H "Origin: https://$BASE" -H "Content-Type: application/json" \
  -X POST "https://$BASE/api/keys" -d '{"name":"smoke-test-key"}')"
key="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["api_key"])' "$mint_body" 2>/dev/null || true)"
key_id="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["id"])' "$mint_body" 2>/dev/null || true)"
if [ -z "$key" ]; then
  echo "FAIL  key mint returned no api_key: $mint_body"
  echo "passed $pass, failed $((fails+1))"; exit $((fails+1))
fi
pass=$((pass+1)); printf 'ok    key minted for %s\n' "$owner"
expect 200 "$BASE" /api/sites -H "X-API-Key: $key" -H "X-Skill-Version: $SKILL_VERSION"
expect 401 "$BASE" /api/sites -H "X-API-Key: not-the-key" -H "X-Skill-Version: $SKILL_VERSION"
curl -sS -b "$admin_jar" -H "Origin: https://$BASE" -X DELETE "https://$BASE/api/keys/$key_id" >/dev/null
expect 401 "$BASE" /api/sites -H "X-API-Key: $key" -H "X-Skill-Version: $SKILL_VERSION"

echo "== a session cookie authenticates the same X-API-Key-shaped routes"
expect 200 "$BASE" /api/sites -b "$admin_jar"

echo "== an authenticated base-host response carries Referrer-Policy and Cache-Control: no-store (review findings)"
expect_header_value "$BASE" /api/sites Referrer-Policy strict-origin-when-cross-origin -b "$admin_jar"
expect_header_value "$BASE" /api/sites Cache-Control no-store -b "$admin_jar"

echo "== sessions page and self-revoke"
expect 200 "$BASE" /auth/sessions -b "$admin_jar"

echo "== a second key for the rest of this run (the first is revoked)"
mint_body="$(curl -sS -b "$admin_jar" -H "Origin: https://$BASE" -H "Content-Type: application/json" \
  -X POST "https://$BASE/api/keys" -d '{"name":"smoke-publish-key"}')"
key="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["api_key"])' "$mint_body" 2>/dev/null || true)"

echo "== publish"
name="smoke-$$"
mkdir -p "$work/site"
printf '<!doctype html><title>%s</title><h1>smoke %s</h1>\n' "$name" "$(date -u +%FT%TZ)" > "$work/site/index.html"
tar -czf "$work/site.tar.gz" -C "$work/site" .
expect 201 "$BASE" "/api/collaboration/sites/$owner/$name" -X POST \
  -H "X-API-Key: $key" -H "X-Skill-Version: $SKILL_VERSION" -H "Content-Type: application/gzip" \
  --data-binary "@$work/site.tar.gz"

label="$(printf '%s' "$owner" | tr '[:upper:]' '[:lower:]' | tr . -)"
owner_host="$label.$BASE"
site_host="$name.$label.$BASE"
dash_host="$label--$name.$BASE"

echo "== the base host no longer serves site content or the site-facing API"
expect 404 "$BASE" "/$name/"
expect 404 "$BASE" "/api/sites/$owner/$name/state"
expect 404 "$BASE" "/api/site/state"

echo "== the owner host $owner_host serves its index behind a session, redirects the old site addresses, and nothing else"
expect 401 "$owner_host" "/"
expect 404 "$owner_host" "/api/sites"
expect 404 "$owner_host" "/api/site/state"
expect 404 "$owner_host" "/sites/$name/"
expect 200 "$owner_host" "/healthz"

# hand_off_session <base-jar-with-a-signed-in-session> <jar-to-fill> <host> <path>
#
# Drives the full session hand-off with curl: the target
# host's own first hop (an unauthenticated navigation) sets the nonce
# cookie and redirects to the base host's own /auth/handoff; that mints a
# one-time code, authenticated by the caller's base session, and redirects
# to the target host's own /auth/session, which redeems the code against
# the nonce cookie and sets that host's own session cookie. Leaves a usable
# session in <jar>. Works identically for an owner host and a site's own
# host — both answer /auth/session the same way.
hand_off_session() {
  local base_jar="$1" jar="$2" host="$3" path="$4"
  local hdr1 hdr2 hdr3 handoff_url session_url

  hdr1="$work/hdr-handoff-1"; rm -f "$jar" "$hdr1"
  curl -sS -c "$jar" -D "$hdr1" -o /dev/null -H "Accept: text/html" "https://$host$path"
  handoff_url="$(grep -i '^location:' "$hdr1" | tail -1 | sed 's/^[Ll]ocation: //' | tr -d '\r')"
  if [ -z "$handoff_url" ]; then echo "  hand_off_session: $host$path gave no redirect"; return 1; fi

  hdr2="$work/hdr-handoff-2"
  curl -sS -b "$base_jar" -D "$hdr2" -o /dev/null "$handoff_url"
  session_url="$(grep -i '^location:' "$hdr2" | tail -1 | sed 's/^[Ll]ocation: //' | tr -d '\r')"
  if [ -z "$session_url" ]; then
    echo "  hand_off_session: /auth/handoff gave no redirect"; head -c 300 "$hdr2"; echo
    return 1
  fi

  hdr3="$work/hdr-handoff-3"
  curl -sS -b "$jar" -c "$jar" -D "$hdr3" -o /dev/null "$session_url"
  if ! grep -qi '^set-cookie: __Host-sh_session=' "$hdr3"; then
    echo "  hand_off_session: /auth/session did not set a session cookie"
    return 1
  fi
  return 0
}

# wait_site_host <owner-host> <site> <site-host> — waits until <site-host>
# presents a certificate curl trusts (the owner's *.<owner-host> wildcard is
# issued) and the old address https://<owner-host>/<site>/ has switched from
# the fallback to the redirect. Non-zero after CERT_WAIT seconds.
wait_site_host() {
  local oh="$1" s="$2" sh="$3"
  for _ in $(seq 1 $((CERT_WAIT / 5 + 1))); do
    # Each server replica re-reads which owners are ready every 15 s, so
    # once one replica redirects, give the others time to agree.
    if curl -sS -o /dev/null "https://$sh/healthz" 2>/dev/null && [ "$(curl -sS -o /dev/null -w "$code_fmt" "https://$oh/$s/" 2>/dev/null)" = "301" ]; then sleep 20; return 0; fi
    sleep 5
  done
  return 1
}

echo "== until the owner certificate is ready, the old address keeps serving the site (fallback, no redirect)"
if curl -sS -o /dev/null "https://$site_host/healthz" 2>/dev/null; then
  echo "  (skipped: *.$owner_host already has its certificate, so the fallback cannot be observed on this run)"
else
  expect 401 "$owner_host" "/$name/"
  fallback_jar="$work/cj-fallback"
  check "hand-off mints a session on $owner_host for the fallback" hand_off_session "$admin_jar" "$fallback_jar" "$owner_host" "/$name/"
  expect 200 "$owner_host" "/$name/" -b "$fallback_jar"
fi

echo "== waiting up to ${CERT_WAIT}s for the certificate of *.$owner_host and the switch to the redirect"
if ! check "$site_host presents a valid certificate and the old address redirects" wait_site_host "$owner_host" "$name" "$site_host"; then
  echo "passed $pass, failed $fails"; exit "$fails"
fi

echo "== once it is ready, the old address and the v1.2 dash host redirect to $site_host"
expect 301 "$owner_host" "/$name/"
expect_header_value "$owner_host" "/$name/a/b.html?q=1" Location "https://$site_host/a/b.html?q=1"
expect_header_value "$owner_host" "/$name//evil.example.com" Location "https://$site_host//evil.example.com"
expect_header_value "$owner_host" "/api/sites/$name/state/versioned" Location "https://$site_host/api/sites/$name/state/versioned"
expect 308 "$owner_host" "/api/sites/$name/state/versioned" -X PUT -H "Content-Type: application/json" -d '{}'
expect_header "$owner_host" "/$name/" Cross-Origin-Resource-Policy
expect 301 "$dash_host" "/p?q=1"
expect_header_value "$dash_host" "/p?q=1" Location "https://$site_host/p?q=1"

echo "== viewing hosted content on the site's own host $site_host requires a session"
expect 401 "$site_host" "/"

echo "== the session hand-off round trip"
owner_jar="$work/cj-owner"
check "hand-off mints a session on $site_host" hand_off_session "$admin_jar" "$owner_jar" "$site_host" "/"
expect 200 "$site_host" "/" -b "$owner_jar"

echo "== every site-host response carries the CORP and Cache-Control headers"
expect_header "$site_host" "/" Cross-Origin-Resource-Policy
expect_header_value "$site_host" "/" Cache-Control "private, no-cache" -b "$owner_jar"

echo "== a sibling-origin subresource load is refused (the owner's own host and other sites are siblings now); a navigation still works"
expect 403 "$site_host" "/" -b "$owner_jar" -H "Sec-Fetch-Site: same-site" -H "Sec-Fetch-Dest: script"
expect 403 "$site_host" "/" -b "$owner_jar" -H "Sec-Fetch-Site: same-site" -H "Sec-Fetch-Dest: script" -H "Referer: https://$owner_host/"
expect 200 "$site_host" "/" -b "$owner_jar" -H "Sec-Fetch-Site: same-site" -H "Sec-Fetch-Dest: document"

echo "== hosted-content auth advances sessions.last_seen_at (review finding)"
owner_id="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["id"])' "$admin_me" 2>/dev/null || true)"
if [ -n "$owner_id" ] && db_query "SELECT 1" >/dev/null 2>&1; then
  db_query "UPDATE sessions SET last_seen_at = now() - interval '10 minutes' WHERE user_id = '$owner_id'::uuid" >/dev/null
  before="$(db_query "SELECT max(last_seen_at) FROM sessions WHERE user_id = '$owner_id'::uuid")"
  curl -sS -b "$owner_jar" -o /dev/null "https://$site_host/"
  after="$(db_query "SELECT max(last_seen_at) FROM sessions WHERE user_id = '$owner_id'::uuid")"
  check "hosted-content request advanced sessions.last_seen_at" bash -c "[ -n '$after' ] && [ '$after' != '$before' ]"
else
  echo "  (skipped: could not resolve the admin's user id or reach Postgres directly from this script)"
fi

echo "== state on the site host, by session and by X-API-Key"
expect 200 "$site_host" "/api/sites/$name/state/versioned" -b "$owner_jar"
put_body="$(curl -sS -b "$owner_jar" -H "Origin: https://$site_host" -H "Content-Type: application/json" \
  -X PUT "https://$site_host/api/sites/$name/state/versioned" -d '{"version":0,"state":{"hello":"world"}}')"
check "session state write returns version 1" bash -c "printf '%s' '$put_body' | grep -q '\"version\":1'"
get_body="$(curl -sS -b "$owner_jar" "https://$site_host/api/sites/$name/state/versioned")"
check "state read reflects the session write" bash -c "printf '%s' '$get_body' | grep -q '\"hello\":\"world\"'"
echo "== PUT with a session but no Origin is refused (Origin required on non-safe methods)"
expect 403 "$site_host" "/api/sites/$name/state/versioned" -b "$owner_jar" -X PUT -H "Content-Type: application/json" -d '{"version":1,"state":{}}'
echo "== an X-API-Key needs no Origin at all"
key_put_body="$(curl -sS -H "X-API-Key: $key" -H "Content-Type: application/json" \
  -X PUT "https://$site_host/api/sites/$name/state/versioned" -d '{"version":1,"state":{"via":"key"}}')"
check "key state write returns version 2" bash -c "printf '%s' '$key_put_body' | grep -q '\"version\":2'"
expect 401 "$site_host" "/api/sites/$name/state/versioned"

echo "== assets: upload, list, serve headers, attachment disposition"
printf 'hello asset world\n' > "$work/asset.txt"
asset_upload_body="$(curl -sS -b "$owner_jar" -H "Origin: https://$site_host" \
  -F "file=@$work/asset.txt;filename=notes.txt;type=text/plain" \
  "https://$site_host/api/sites/$name/assets")"
asset_id="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["id"])' "$asset_upload_body" 2>/dev/null || true)"
check "asset upload returned an id" bash -c "[ -n '$asset_id' ]"
if [ -n "$asset_id" ]; then
  expect 200 "$site_host" "/_assets/$asset_id/notes.txt" -b "$owner_jar"
  expect_header "$site_host" "/_assets/$asset_id/notes.txt" Content-Disposition -b "$owner_jar"
  expect_header "$site_host" "/_assets/$asset_id/notes.txt" X-Content-Type-Options -b "$owner_jar"
  asset_list_body="$(curl -sS -b "$owner_jar" "https://$site_host/api/sites/$name/assets")"
  check "asset appears in the list" bash -c "printf '%s' '$asset_list_body' | grep -q '\"notes.txt\"'"

  echo "== the dashboard's own asset admin route sees the same asset (base host, owner session)"
  expect 200 "$BASE" "/dashboard" -b "$admin_jar"
  dashboard_asset_list="$(curl -sS -b "$admin_jar" -H "X-Simple-Host-Client: control-ui" \
    "https://$BASE/api/collaboration/sites/$owner/$name/assets")"
  check "dashboard asset list includes the uploaded asset" bash -c "printf '%s' '$dashboard_asset_list' | grep -q '\"notes.txt\"'"
  curl -sS -b "$admin_jar" -c "$admin_jar" -H "Origin: https://$BASE" -H "X-Simple-Host-Client: control-ui" \
    -X DELETE "https://$BASE/api/collaboration/sites/$owner/$name/assets/$asset_id" -o /dev/null
  expect 404 "$site_host" "/_assets/$asset_id/notes.txt" -b "$owner_jar"
fi

echo "== an archive with a top-level _assets/ entry is refused at upload (pen item)"
mkdir -p "$work/bad-site/_assets"
printf '<!doctype html><title>bad</title>\n' > "$work/bad-site/index.html"
printf 'shadowing the asset route\n' > "$work/bad-site/_assets/evil.txt"
tar -czf "$work/bad-site.tar.gz" -C "$work/bad-site" .
expect 400 "$BASE" "/api/collaboration/sites/$owner/$name-badassets" -X POST \
  -H "X-API-Key: $key" -H "X-Skill-Version: $SKILL_VERSION" -H "Content-Type: application/gzip" \
  --data-binary "@$work/bad-site.tar.gz"

echo "== an asset that sniffs as HTML is refused regardless of its declared type or extension (pen item: content-type confusion)"
printf '<html><body>not really text</body></html>\n' > "$work/fake.txt"
expect 415 "$site_host" "/api/sites/$name/assets" -b "$owner_jar" -H "Origin: https://$site_host" \
  -F "file=@$work/fake.txt;filename=fake.txt;type=text/plain"

echo "== every state and asset write lands a real audit_events row"
site_id="$(db_query "SELECT id FROM sites WHERE user_id = '$owner_id'::uuid AND name = '$name'" 2>/dev/null)"
if [ -n "$site_id" ] && db_query "SELECT 1" >/dev/null 2>&1; then
  # The session write (version 1) and the key write (version 2) above are
  # the same actor (the admin's own key), a few seconds apart, so design
  # 8.1's five-minute coalescing window must land them in one row with
  # detail.count >= 2 rather than two separate rows.
  state_write_rows="$(db_query "SELECT count(*) FROM audit_events WHERE action = 'state_write' AND site_id = '$site_id'::uuid")"
  state_write_count="$(db_query "SELECT COALESCE((detail->>'count')::int, 1) FROM audit_events WHERE action = 'state_write' AND site_id = '$site_id'::uuid")"
  asset_create_rows="$(db_query "SELECT count(*) FROM audit_events WHERE action = 'asset_create' AND site_id = '$site_id'::uuid")"
  asset_delete_rows="$(db_query "SELECT count(*) FROM audit_events WHERE action = 'asset_delete' AND site_id = '$site_id'::uuid")"
  check "exactly one coalesced state_write row for this site" bash -c "[ '$state_write_rows' = '1' ]"
  check "the coalesced state_write row's detail.count is at least 2" bash -c "[ '$state_write_count' -ge 2 ]"
  check "an asset_create audit_events row exists" bash -c "[ '$asset_create_rows' -ge 1 ]"
  check "an asset_delete audit_events row exists" bash -c "[ '$asset_delete_rows' -ge 1 ]"
else
  echo "  (skipped: could not resolve this run's site id or reach Postgres directly from this script)"
fi

echo "== every hosted-content view is logged to access_log, including the owner's own"
if [ -n "$site_id" ] && db_query "SELECT 1" >/dev/null 2>&1; then
  # $owner_jar viewed $site_host/ several times above; the
  # isSelfTraffic exclusion is documented as applying only to the
  # pageview/download analytics counters, never to access_log itself.
  # AccessWriter batches on a 2-second flush interval (access_writer.go),
  # so give it a moment before reading the table back.
  sleep 3
  access_rows="$(db_query "SELECT count(*) FROM access_log WHERE owner_label = '$owner' AND site_name = '$name'")"
  check "access_log carries at least one row for this run's owner view" bash -c "[ '$access_rows' -ge 1 ]"
else
  echo "  (skipped: could not resolve this run's site id or reach Postgres directly from this script)"
fi

echo "== restricting a site keeps it on its own host"
curl -sS -b "$admin_jar" -H "Origin: https://$BASE" -H "Content-Type: application/json" -X POST "https://$BASE/api/collaboration/sites/$owner/$name/viewers" -d "{\"usernames\":[\"$owner\"]}" >/dev/null
restricted_host="$site_host"
restricted_url="$(curl -sS -b "$admin_jar" "https://$BASE/api/collaboration/sites/$owner/$name" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("url",""))' 2>/dev/null)"
check "the restricted site's url is still https://$site_host/" bash -c "[ '$restricted_url' = 'https://$site_host/' ]"
# The old address still redirects: computed from the address alone, it
# confirms nothing about the site or its viewers.
expect 301 "$owner_host" "/$name/"

echo "== a host session cookie is refused on a different host, even the same owner's (pen item: session fixation across hosts)"
# owner_index_jar's cookie is minted for $owner_host (the owner's index) and
# admin is a listed viewer of the now-restricted site — without the host
# binding this would wrongly serve 200 on $site_host too, since the session
# and the viewer grant are both otherwise valid; the host mismatch alone
# must refuse it.
owner_index_jar="$work/cj-owner-index"
check "hand-off mints a session on the owner index $owner_host" hand_off_session "$admin_jar" "$owner_index_jar" "$owner_host" "/"
expect 200 "$owner_host" "/" -b "$owner_index_jar"
expect 401 "$site_host" "/" -b "$owner_index_jar"
expect 401 "$owner_host" "/" -b "$owner_jar"

person_restricted_jar="$work/cj-person-restricted"
if [ -n "$person_username" ] && [ -f "$person_jar" ]; then
  check "hand-off to the restricted host still succeeds for a non-listed signed-in user" \
    hand_off_session "$person_jar" "$person_restricted_jar" "$restricted_host" "/"
  expect 404 "$restricted_host" "/" -b "$person_restricted_jar"

  curl -sS -b "$admin_jar" -H "Origin: https://$BASE" -H "Content-Type: application/json" \
    -X POST "https://$BASE/api/collaboration/sites/$owner/$name/viewers" -d "{\"usernames\":[\"$person_username\"]}" >/dev/null
  expect 200 "$restricted_host" "/" -b "$person_restricted_jar"
fi
expect_header "$restricted_host" "/" Cross-Origin-Resource-Policy

echo "== state on the restricted site's own host, by session, nameless shape"
restricted_owner_jar="$work/cj-owner-restricted"
check "hand-off to the restricted host for its own owner" \
  hand_off_session "$admin_jar" "$restricted_owner_jar" "$restricted_host" "/"
restricted_get_body="$(curl -sS -b "$restricted_owner_jar" "https://$restricted_host/api/site/state/versioned")"
current_version="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["version"])' "$restricted_get_body" 2>/dev/null || echo 0)"
next_version=$((current_version + 1))
restricted_put_body="$(curl -sS -b "$restricted_owner_jar" -H "Origin: https://$restricted_host" -H "Content-Type: application/json" \
  -X PUT "https://$restricted_host/api/site/state/versioned" -d "{\"version\":$current_version,\"state\":{\"restricted\":true}}")"
check "restricted-host state write succeeds" bash -c "printf '%s' '$restricted_put_body' | grep -q '\"version\":$next_version'"
echo "== the named shape also works on the restricted host, naming its own site"
expect 200 "$restricted_host" "/api/sites/$name/state/versioned" -b "$restricted_owner_jar"
echo "== the named shape naming a different site is refused, not redirected (pen item: state write across sites)"
expect 404 "$restricted_host" "/api/sites/some-other-site/state" -b "$restricted_owner_jar"
echo "== the base host never answers a state or asset route for any site (pen item: state write across sites)"
expect 404 "$BASE" "/api/sites/$name/state"
expect 404 "$BASE" "/api/sites/$name/assets"

echo "== GET /api/audit and GET /api/access"
audit_body="$(curl -sS -b "$admin_jar" -H "X-Simple-Host-Client: control-ui" "https://$BASE/api/audit?owner=$owner&site=$name")"
check "GET /api/audit as the owner sees this run's state_write action" bash -c "printf '%s' '$audit_body' | grep -q '\"action\":\"state_write\"'"
if [ -n "$person_username" ]; then
  escaped_owner="$(python3 -c 'import urllib.parse,sys; print(urllib.parse.quote(sys.argv[1]))' "$owner")"
  person_audit_body="$(curl -sS -b "$person_jar" -H "X-Simple-Host-Client: control-ui" "https://$BASE/api/audit?owner=$escaped_owner")"
  check "GET /api/audit as a stranger to \$owner carries none of their rows (pen item: export scope escape)" \
    bash -c "! printf '%s' '$person_audit_body' | grep -q '\"owner_id\":\"$owner_id\"'"
fi
access_body="$(curl -sS -b "$admin_jar" -H "X-Simple-Host-Client: control-ui" "https://$BASE/api/access?owner=$owner&site=$name")"
check "GET /api/access as the owner sees this run's view" bash -c "printf '%s' '$access_body' | grep -q '\"site_name\":\"$name\"'"
if [ -n "$person_username" ]; then
  person_access_body="$(curl -sS -o /dev/null -w '%{http_code}' -b "$person_jar" -H "X-Simple-Host-Client: control-ui" "https://$BASE/api/access?owner=$owner&site=$name")"
  check "GET /api/access as a stranger to \$owner is refused" bash -c "[ '$person_access_body' = '403' ]"

  # $owner (the admin test account) is an admin, so admin_jar's own read
  # above always takes the Admin path (ip included, by design) regardless
  # of the owner= filter — it cannot exercise the owner-scope redaction.
  # Prove that on a real non-admin owner instead: person publishes and
  # views a site of their own, then reads their own access log back.
  person_key_body="$(curl -sS -b "$person_jar" -H "Origin: https://$BASE" -H "Content-Type: application/json" \
    -X POST "https://$BASE/api/keys" -d '{"name":"smoke-person-key"}')"
  person_key="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["api_key"])' "$person_key_body" 2>/dev/null || true)"
  person_site="smoke-person-$$"
  mkdir -p "$work/person-site"
  printf '<!doctype html><title>%s</title>\n' "$person_site" > "$work/person-site/index.html"
  tar -czf "$work/person-site.tar.gz" -C "$work/person-site" .
  person_create_status="$(curl -sS -o /dev/null -w '%{http_code}' -X POST \
    -H "X-API-Key: $person_key" -H "X-Skill-Version: $SKILL_VERSION" -H "Content-Type: application/gzip" \
    --data-binary "@$work/person-site.tar.gz" "https://$BASE/api/collaboration/sites/$person_username/$person_site")"
  if [ "$person_create_status" = "201" ]; then
    person_label="$(printf '%s' "$person_username" | tr '[:upper:]' '[:lower:]' | tr . -)"
    person_site_host="$person_site.$person_label.$BASE"
    check "$person_site_host becomes usable" wait_site_host "$person_label.$BASE" "$person_site" "$person_site_host"
    person_owner_jar="$work/cj-person-owner"
    check "hand-off mints a session on $person_site_host" hand_off_session "$person_jar" "$person_owner_jar" "$person_site_host" "/"
    curl -sS -b "$person_owner_jar" -o /dev/null "https://$person_site_host/"
    sleep 3
    person_own_access_body="$(curl -sS -b "$person_jar" -H "X-Simple-Host-Client: control-ui" "https://$BASE/api/access?owner=$person_username&site=$person_site")"
    check "GET /api/access as a real non-admin owner sees counts of their own views" \
      bash -c "printf '%s' '$person_own_access_body' | grep -q '\"unique_viewers\":1'"
    check "GET /api/access owner scope carries no identities (ACCESS_LOG_VISIBILITY=counts)" \
      bash -c "! printf '%s' '$person_own_access_body' | grep -q '\"ip\":\\|\"user_id\":'"
  else
    echo "  (skipped the owner-scope-has-no-ip check: could not publish a site for the non-admin test account, status $person_create_status)"
  fi
fi

echo "== GET /api/admin/export streams CSV and JSONL, admin only, itself audited"
expect 403 "$BASE" "/api/admin/export?kind=audit&format=jsonl" -b "$person_jar" -H "X-Simple-Host-Client: control-ui"
export_csv="$(curl -sS -b "$admin_jar" -H "X-Simple-Host-Client: control-ui" "https://$BASE/api/admin/export?kind=audit&format=csv")"
check "the CSV export starts with its header row" bash -c "printf '%s' '$export_csv' | head -1 | grep -q '^id,at,request_id,'"
export_jsonl="$(curl -sS -b "$admin_jar" -H "X-Simple-Host-Client: control-ui" "https://$BASE/api/admin/export?kind=access&format=jsonl")"
check "every JSONL export line is a JSON object" bash -c "printf '%s\n' '$export_jsonl' | head -3 | python3 -c 'import json,sys
for line in sys.stdin:
    line = line.strip()
    if line:
        json.loads(line)'"
if [ -n "$site_id" ] && db_query "SELECT 1" >/dev/null 2>&1; then
  admin_export_rows="$(db_query "SELECT count(*) FROM audit_events WHERE action = 'admin_export'")"
  check "the export itself is audited as admin_export" bash -c "[ '$admin_export_rows' -ge 1 ]"
fi

echo "== simple-host prune -dry-run lists partitions without dropping them"
prune_job="smoke-prune-$$"
# A Job's pod template is immutable once created, so the "-dry-run" flag has
# to go in before the Job object exists: render it client-side, patch args
# in the rendered JSON with python3, then create the patched object.
if kubectl --context "$CLUSTER_CONTEXT" -n "$NAMESPACE" create job "$prune_job" --from=cronjob/simple-host-prune \
    --dry-run=client -o json > "$work/prune-job.json" 2>/dev/null; then
  python3 -c '
import json, sys
path = sys.argv[1]
with open(path) as f:
    job = json.load(f)
job["spec"]["template"]["spec"]["containers"][0]["args"] = ["prune", "-dry-run"]
with open(path, "w") as f:
    json.dump(job, f)
' "$work/prune-job.json"
  kubectl --context "$CLUSTER_CONTEXT" -n "$NAMESPACE" create -f "$work/prune-job.json" >/dev/null 2>&1
  kubectl --context "$CLUSTER_CONTEXT" -n "$NAMESPACE" wait --for=condition=complete "job/$prune_job" --timeout=60s >/dev/null 2>&1
  prune_pod="$(kubectl --context "$CLUSTER_CONTEXT" -n "$NAMESPACE" get pods -l job-name="$prune_job" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
  prune_log="$(kubectl --context "$CLUSTER_CONTEXT" -n "$NAMESPACE" logs "$prune_pod" 2>/dev/null)"
  check "prune -dry-run reports its partition decision" bash -c "printf '%s' '$prune_log' | grep -q 'prune -dry-run:'"
  kubectl --context "$CLUSTER_CONTEXT" -n "$NAMESPACE" delete job "$prune_job" --ignore-not-found >/dev/null 2>&1
else
  echo "  (skipped: could not render a one-off Job from cronjob/simple-host-prune)"
fi

echo
echo "passed $pass, failed $fails"
exit "$fails"
