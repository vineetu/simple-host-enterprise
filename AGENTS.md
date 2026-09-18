# AGENTS.md

Guidance for coding agents working in this repository. `CLAUDE.md` holds the
same rules in full; this file exists for agents that read this name.

- Read `docs/security-review.md` and `docs/configuration.md` before
  changing anything that touches credentials, the schema, or a refusal.
- Verify against the code, not older docs; cite file paths and line numbers.
- Test before committing: `make test`, and `make local && make smoke` for
  anything that touches serving, routing, the schema, or the manifests.
- Short commit messages; no coding-agent or model-vendor references in them.
- Never commit credentials. `SCRUB.md` lists what must never reappear.
- A management API change is also an `internal/mcp/tools.go` change and a
  skill-text change, in the same commit.
