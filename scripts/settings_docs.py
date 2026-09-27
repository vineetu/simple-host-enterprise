#!/usr/bin/env python3
"""Keep the docs that describe settings in step with docs/advanced/settings.json.

The same script lives in both repos (simple-host and simple-host-enterprise).

docs/advanced/settings.json is generated from the code (`simple-host settings
--json`; a Go test fails when it drifts). This script:

  * fills every settings table in docs/advanced/*.md from it, between
    <!-- settings:group=<id> --> (or settings:type=<type>) and <!-- /settings -->;
  * checks docs/configuration.md names every setting and states the same
    default (and range, where its row gives one), and names nothing else;
  * hosted repo: checks every setting offered on a small box is passed through
    compose.yaml and kept by deploy/install/install.sh, and keeps the setup
    helper's copies (internal/handler/static/setup/*.json) equal to the
    sources: this repo's settings.json, and the enterprise repo's when a
    checkout of it is found ($ENTERPRISE_REPO, ../simple-host-enterprise or
    /tmp/ent-wt/advanced).

  python3 scripts/settings_docs.py          rewrite the tables and copies
  python3 scripts/settings_docs.py --check  fail if anything is out of step
"""
import json
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CHECK = "--check" in sys.argv[1:]
errors = []


def read(path):
    with open(os.path.join(ROOT, path), encoding="utf-8") as f:
        return f.read()


def write_or_check(path, new):
    full = os.path.join(ROOT, path)
    old = open(full, encoding="utf-8").read() if os.path.exists(full) else None
    if old == new:
        return
    if CHECK:
        errors.append(f"{path} is out of step with docs/advanced/settings.json (run scripts/sync-settings.sh)")
    else:
        with open(full, "w", encoding="utf-8") as f:
            f.write(new)
        print(f"updated {path}")


doc = json.loads(read("docs/advanced/settings.json"))
settings = doc["settings"]
by_name = {s["name"]: s for s in settings}


def allowed(s):
    parts = []
    if s.get("allowed"):
        parts.append(" / ".join(f"`{a}`" for a in s["allowed"]))
    elif "min" in s or "max" in s:
        lo, hi = s.get("min"), s.get("max")
        rng = f"{lo}–{hi}" if lo is not None and hi is not None else (f"at least {lo}" if lo is not None else f"at most {hi}")
        parts.append(rng + (f" {s['unit']}" if s.get("unit") else ""))
    elif s.get("unit"):
        parts.append(s["unit"])
    if s["type"] == "rate":
        parts.append(f"stricter freely; loosest `{s['loosest']}`" if s.get("loosest") else "any (warns past 10× looser)")
    if s["type"] == "secret":
        parts.append("secret")
    return "; ".join(parts) or "text"


def cell(text):
    return text.replace("|", "\\|")


def table(rows):
    out = ["| Setting | Default | Allowed | What it does |", "|---|---|---|---|"]
    for s in rows:
        default = f"`{s['default']}`" if s["default"] else "none"
        desc = s["description"]
        if s["required"]:
            desc += " **Required.**"
        if s["security_sensitive"]:
            desc += " **Security-sensitive.**"
        out.append(f"| `{s['name']}` | {cell(default)} | {cell(allowed(s))} | {cell(desc)} |")
    return "\n".join(out)


MARK = re.compile(r"(<!-- settings:(group|type)=([a-z-]+) -->\n)(.*?)(<!-- /settings -->)", re.S)

adv = os.path.join(ROOT, "docs/advanced")
covered = set()
for name in sorted(os.listdir(adv)):
    if not name.endswith(".md"):
        continue
    path = f"docs/advanced/{name}"
    text = read(path)

    def fill(m):
        kind, value = m.group(2), m.group(3)
        rows = [s for s in settings if s[kind] == value]
        if not rows:
            errors.append(f"{path}: no setting has {kind} {value}")
        if kind == "group":
            covered.update(s["name"] for s in rows)
        return m.group(1) + table(rows) + "\n" + m.group(5)

    write_or_check(path, MARK.sub(fill, text))

for g in doc["groups"]:
    if not os.path.exists(os.path.join(adv, g["page"])):
        errors.append(f"docs/advanced/{g['page']} (the {g['name']} page) is missing")
for s in settings:
    if s["name"] not in covered:
        errors.append(f"{s['name']} is in no docs/advanced table (add <!-- settings:group={s['group']} --> to its page)")

# docs/configuration.md is the full reference: every setting, the same default.
conf = read("docs/configuration.md")
rows, section = {}, ""
for line in conf.splitlines():
    if line.startswith("#"):
        section = line
        continue
    m = re.match(r"^\| `([A-Z][A-Z0-9_]+)`(?:, `[A-Z0-9_]+`)* \|", line)
    if m and not re.search(r"Removed|Certificate issuers", section):
        for n in re.findall(r"`([A-Z][A-Z0-9_]+)`", line.split("|")[1]):
            rows[n] = line
for s in settings:
    row = rows.get(s["name"])
    if row is None:
        errors.append(f"docs/configuration.md has no table row for {s['name']}")
        continue
    d = s["default"]
    cols = [c.strip() for c in row.split("|")[2:]]
    if d and not d.startswith("<") and f"`{d}`" not in row and d not in cols:
        errors.append(f"docs/configuration.md: {s['name']} default is {d!r} in code but the row says otherwise")
    lo, hi = s.get("min"), s.get("max")
    if isinstance(lo, int) and isinstance(hi, int) and re.search(r"\| \d[\d,]*–\d", row):
        if f"| {lo}–{hi} |" not in row:
            errors.append(f"docs/configuration.md: {s['name']} range is {lo}–{hi} in code but the row says otherwise")
    if s.get("loosest") and s["loosest"] not in row:
        errors.append(f"docs/configuration.md: {s['name']} loosest value is {s['loosest']} in code but the row says otherwise")
for n in rows:
    if n not in by_name:
        errors.append(f"docs/configuration.md has a row for {n}, which the server does not read")

# Hosted: what the setup helper offers for a small box must work there.
if os.path.exists(os.path.join(ROOT, "compose.yaml")):
    compose = read("compose.yaml")
    install = read("deploy/install/install.sh")
    kept = set(re.findall(r"[A-Z][A-Z0-9_]+", " ".join(re.findall(r'^LIMIT_VARS\+="([^"]*)"', install, re.M))))
    for s in settings:
        if not s["small_box"] or s.get("install_flag"):
            continue
        if not re.search(rf"^\s+{s['name']}: \$\{{{s['name']}[:}}-]", compose, re.M):
            errors.append(f"{s['name']} is offered on a small box but compose.yaml does not pass it through")
        if s["name"] not in kept and not s["name"].startswith("RATE_LIMIT_"):
            errors.append(f"{s['name']} is offered on a small box but install.sh does not keep it on a re-run (LIMIT_VARS)")

    static = "internal/handler/static/setup"
    write_or_check(f"{static}/small-box-settings.json", read("docs/advanced/settings.json"))
    candidates = [os.environ.get("ENTERPRISE_REPO", ""), os.path.join(ROOT, "..", "simple-host-enterprise"), "/tmp/ent-wt/advanced"]
    ent = next((c for c in candidates if c and os.path.exists(os.path.join(c, "docs/advanced/settings.json"))), None)
    if ent:
        with open(os.path.join(ent, "docs/advanced/settings.json"), encoding="utf-8") as f:
            write_or_check(f"{static}/enterprise-settings.json", f.read())
    elif not os.path.exists(os.path.join(ROOT, static, "enterprise-settings.json")):
        errors.append(f"{static}/enterprise-settings.json is missing and no enterprise checkout was found (set ENTERPRISE_REPO)")
    else:
        print("settings_docs: no enterprise checkout found; enterprise-settings.json not compared (set ENTERPRISE_REPO)")

if errors:
    print("\n".join(errors), file=sys.stderr)
    sys.exit(1)
print(f"settings_docs: {len(settings)} settings, docs in step")
