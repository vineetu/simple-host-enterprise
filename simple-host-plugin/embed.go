// Package plugin embeds the skill bundle so the simple_host server can:
//
//   - Serve /skills.zip (a flat zip of skill folders) that an agent
//     downloads, verifies against the published SHA-256, and extracts
//     into the active agent root: ~/.agents/skills (current ChatGPT desktop app),
//     ~/.claude/skills (Claude Code), or the root another agent reports.
//     Existing ~/.codex/skills copies remain supported as legacy installs.
//   - Serve /skills/version ({version, sha256}) for the agent's
//     version-check and integrity verification.
//
// Install is agent-driven and pure HTTPS — no git, no npm, no registry,
// no install script. See docs/secure-skill-install/design.md.
package plugin

import "embed"

//go:embed all:skills all:.claude-plugin
var FS embed.FS
