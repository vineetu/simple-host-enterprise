// Package plugin embeds the skill bundle so the simple_host server can:
//
//   - Serve /skills.zip (a flat zip of skill folders) that an agent
//     downloads, verifies against the published SHA-256, and extracts
//     into the active agent root: ~/.agents/skills (current ChatGPT desktop app),
//     ~/.claude/skills (Claude Code), or the root another agent reports.
//     Existing ~/.codex/skills copies remain supported as legacy installs.
//   - Serve /plugin.zip: the same skills plus the Claude plugin manifest
//     (.claude-plugin/plugin.json, .mcp.json) and the Agent Plugins one
//     (plugin.json, mcp.json), with {{BASE_URL}} filled in at serve time so
//     the plugin points at this instance's /mcp.
//   - Serve /skills/version ({version, sha256}) for the agent's
//     version-check and integrity verification.
//
// Install is agent-driven and pure HTTPS — no git, no npm, no registry,
// no install script.
package plugin

import "embed"

//go:embed all:skills all:.claude-plugin .mcp.json plugin.json mcp.json
var FS embed.FS
