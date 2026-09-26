#!/bin/sh
# Fails when a registered HTTP route pattern or an MCP tool name is missing
# from FEATURES.md. Routes are the literal "METHOD /pattern" strings passed to
# mux.Handle/HandleFunc; tools are the Name: fields in internal/mcp/tools.go.
# Each must appear in FEATURES.md in backticks.
set -eu
cd "$(dirname "$0")/.."

missing=0
routes=$(grep -rhoE --include='*.go' --exclude='*_test.go' 'Handle(Func)?\("[A-Z]+ /[^"]*"' cmd internal |
	sed -E 's/^Handle(Func)?\("//; s/"$//' | sort -u)
tools=$(grep -hoE '^[[:space:]]*Name:[[:space:]]*"[a-z_]+"' internal/mcp/tools.go |
	sed -E 's/.*"([a-z_]+)"/\1/' | sort -u)

[ -n "$routes" ] || { echo "check-features: found no routes; the pattern needs updating"; exit 1; }
[ -n "$tools" ] || { echo "check-features: found no MCP tools; the pattern needs updating"; exit 1; }

IFS='
'
for r in $routes; do
	grep -qF "\`$r\`" FEATURES.md || { echo "FEATURES.md is missing route: $r"; missing=1; }
done
for t in $tools; do
	grep -qF "\`$t\`" FEATURES.md || { echo "FEATURES.md is missing MCP tool: $t"; missing=1; }
done

[ $missing -eq 0 ] || { echo "Add them to FEATURES.md in the same commit."; exit 1; }
echo "check-features: $(echo "$routes" | wc -l | tr -d ' ') routes, $(echo "$tools" | wc -l | tr -d ' ') MCP tools, all in FEATURES.md"
