#!/usr/bin/env bash
# Runs the questions block of deploy/terraform/aws/apply.sh (between
# "# <ask>" and "# </ask>") at a pseudo-terminal, with no AWS account:
#   1. everything in the line                     -> nothing asked
#   2. only region and create_cluster (all empty) -> asked in order, answers in terraform.tfvars
#   3. a wrong answer                             -> says why and asks again
#   4. Google as the issuer                       -> company email domains asked
#   5. your own cluster                           -> its name asked
#   6. empty answer with a default                -> the default
#   7. no terminal                                -> stops with the list
#   8. --yes                                      -> stops with the list
#   9. a value in TF_VAR_<name>                    -> not asked
# Needs bash and python3. Usage: bash scripts/test-ask.sh
set -euo pipefail
here=$(cd "$(dirname "$0")/.." && pwd)
block=$(sed -n '/# <ask>/,/# <\/ask>/p' "$here/deploy/terraform/aws/apply.sh")
var=$(sed -n '/^var() {/,/^}/p' "$here/deploy/terraform/aws/apply.sh")
[ -n "$block" ] && [ -n "$var" ] || { echo "FAIL: the <ask> block or var() is missing from apply.sh"; exit 1; }
harness=$(mktemp); trap 'rm -f "$harness"' EXIT
# The block's context: die(), say(), drain(), var(), $yes and $tfvars; it
# prints terraform.tfvars at the end.
cat > "$harness" <<EOF
set -euo pipefail
die() { printf '\nStopped: %s\n' "\$*"; exit 1; }
say() { printf '\n== %s\n' "\$*"; }
drain() { :; }
yes=\${ASK_YES:-}
tfvars=\$(printf '%s' "\$ASK_TFVARS")
$var
$block
printf '%s\n' '--- tfvars' "\$tfvars"
EOF

# run <tfvars> <answer>...: the block at a pty, each answer typed when a
# question (a line ending in ": ") is waiting; prints everything it wrote.
run() {
  ASK_TFVARS=$1 HARNESS=$harness python3 - "${@:2}" <<'PY'
import os, pty, sys, time, select
answers = sys.argv[1:]
pid, fd = pty.fork()
if pid == 0:
    os.execvp('bash', ['bash', os.environ['HARNESS']])
out = b''
seen = 0
deadline = time.time() + 20
while time.time() < deadline:
    r, _, _ = select.select([fd], [], [], 0.2)
    if r:
        try:
            chunk = os.read(fd, 4096)
        except OSError:
            break
        if not chunk:
            break
        out += chunk
    elif answers and out.rstrip(b' ').endswith(b':') and out.count(b': ') > seen:
        seen = out.count(b': ')
        os.write(fd, answers.pop(0).encode() + b'\r')
os.waitpid(pid, 0)
sys.stdout.write(out.decode(errors='replace').replace('\r', ''))
PY
}
fail=0
expect() {
  local name=$1 want=$2 got=$3
  if grep -qzE "$want" <<<"$got"; then echo "ok   $name"; else echo "FAIL $name: wanted /$want/, got:"; sed 's/^/     /' <<<"$got"; fail=1; fi
}
full='create_cluster = true
region         = "us-east-2"
base_domain    = "sites.acme.com"
admin_emails   = ["a@acme.com"]
oidc_issuer    = "https://acme.okta.com"
oidc_client_id = "0oa1"'
min='create_cluster = true
region         = "us-east-2"'

got=$(run "$full")
expect "1 everything in the line: nothing asked" '^--- tfvars' "$got"

got=$(run "$min" 'HTTPS://Sites.Acme-Sites.COM/' 'Alex@Acme.com, platform@corp' 'acme.okta.com/oauth2/default/' '0oa${x}1')
expect "2 all empty: asked in order" 'Address, like sites.example.com: .*Admin emails, separated by commas: .*Sign-in issuer URL, like https://your-org.okta.com: .*Client ID: ' "$got"
expect "2 answers in terraform.tfvars" 'base_domain = "sites.acme-sites.com"
admin_emails = \["alex@acme.com", "platform@corp"\]
oidc_issuer = "https://acme.okta.com/oauth2/default"
oidc_client_id = "0oa\$\$\{x\}1"' "$got"

got=$(run "$min" 'not a host' 'sites.acme.com' 'a"b@x' 'a@acme.com' 'http://x y' 'https://ACCOUNTS.google.com:443/' 'Acme.com, @corp.example' 'cid')
expect "3 a wrong answer is refused and asked again" 'A hostname, like sites.example.com.*is not an email address.*An https:// URL' "$got"
expect "4 Google: company email domains asked" 'Company email domains \(required with Google\).*allowed_email_domains = \["acme.com", "corp.example"\]' "$got"
expect "4 Google's issuer written exactly" 'oidc_issuer = "https://accounts.google.com"' "$got"

got=$(run 'region = "eu-west-1"
create_cluster = false' 's.acme.com' 'a@acme.com' 'https://acme.okta.com' 'cid' 'prod;id' 'prod-eks')
expect "5 your own cluster: its name asked, checked" "Your EKS cluster's name.*An EKS cluster name.*cluster_name = \"prod-eks\"" "$got"

got=$(run '' '' '' 's.acme.com' 'a@acme.com' 'https://acme.okta.com' 'cid')
expect "6 defaults in brackets, empty takes them" 'Create a new EKS cluster.*\[yes\]: .*AWS region \[us-east-1\]: .*create_cluster = true
region = "us-east-1"' "$got"

got=$(setsid bash -c 'ASK_TFVARS="$1" bash "$2" </dev/null 2>&1' _ "$min" "$harness" || true)
expect "7 no terminal: stops with the list" 'Stopped: these values are not in the line: base_domain, admin_emails, oidc_issuer, oidc_client_id' "$got"

got=$(ASK_YES=1 run "$min")
expect "8 --yes: stops with the list" 'Stopped: these values are not in the line: base_domain' "$got"

got=$(TF_VAR_base_domain=sites.acme.com TF_VAR_admin_emails='["a@acme.com"]' run "$min" 'https://acme.okta.com' 'cid')
expect "9 TF_VAR_ values are not asked" 'Sign-in issuer URL.*Client ID: ' "$got"
if grep -q 'Address, like' <<<"$got"; then echo "FAIL 9: asked for the address given in TF_VAR_base_domain"; fail=1; fi
exit $fail
