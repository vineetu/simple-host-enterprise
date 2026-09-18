# Documentation

The package is built in phases from a plan that lives outside this
repository. What is here:

- `phases/` — one record per completed phase: what was built, how it was
  verified, where it deviated from the plan and why, what the reviewer
  found, and what remains. Newest last. Read the latest before changing
  anything.
- `install.md`, `configuration.md`, `security-review.md` — written in the
  documentation phase; absent until then.

The top-level `README.md` says how to run the package locally, `CLAUDE.md`
and `AGENTS.md` carry the working rules, and `SCRUB.md` is the checklist that
keeps the original instance's identifiers out of this tree.