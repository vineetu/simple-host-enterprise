#!/bin/sh
# Fails when a numbered top-level section of FEATURES.md ("## N. Title") has no
# line in PARITY.md's section index ("| <repo> | Title |"). PARITY.md is
# identical in simple-host and simple-host-enterprise; only REPO differs here.
set -eu
cd "$(dirname "$0")/.."
REPO=enterprise

[ -f PARITY.md ] || { echo "check-parity: PARITY.md is missing"; exit 1; }
titles=$(sed -nE 's/^## [0-9]+\. (.+)$/\1/p' FEATURES.md)
[ -n "$titles" ] || { echo "check-parity: no '## N. Title' sections in FEATURES.md; the pattern needs updating"; exit 1; }

missing=0
IFS='
'
for t in $titles; do
	grep -qF "| $REPO | $t |" PARITY.md || { echo "PARITY.md has no row for FEATURES.md section: $t"; missing=1; }
done
[ $missing -eq 0 ] || { echo "Add a '| $REPO | <section> | <rows> |' line (and the feature row) to PARITY.md, and copy PARITY.md to the other repo."; exit 1; }
echo "check-parity: $(echo "$titles" | wc -l | tr -d ' ') FEATURES.md sections, all in PARITY.md"
