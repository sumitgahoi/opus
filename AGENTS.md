# Working instructions

Read REQUIREMENTS.md before changing code. It contains the project's
scope, architecture, acceptance criteria, and current milestone.

Scale: design for ~1M songs worst case across music-*. Keep inventory,
log, and sync paths fluent at that size (see Scale in REQUIREMENTS.md).

## Working style

- Prefer small, single-purpose, composable code.
- Optimize for correctness, readability, and long-term maintenance.
- Challenge unnecessary complexity.
- Use the Go standard library where practical.
- Add dependencies only for a concrete need; explain the choice briefly.
- Use SQLite directly with explicit SQL and transactions.
- Avoid speculative abstractions, generic sync engines, and frameworks.
- Do not add documentation. REQUIREMENTS.md is the only product
  document. Change it only to record a decision the current milestone
  cannot proceed without. No READMEs, design notes, status reports,
  or comments that restate the code.
- Keep implementation work within the current milestone.
- Proceed autonomously with routine, reversible implementation choices.
- Ask only when ambiguity materially affects correctness or scope.
- Explain the work as a peer. Before and while writing code, say what
  the piece is for, why it has this shape, and what it leaves out, so
  the Go is understandable line by line.
- Do not delegate to subagents unless explicitly requested.

## Project boundaries

This repository belongs to the experimental custom music system.
The separate iPhone OpenSubsonic client must not be redesigned or
compromised to accommodate it.

Shared code may be considered later only when it benefits both projects.
Do not introduce shared infrastructure in anticipation of that.

## Data and correctness

- Never modify, retag, move, or delete source music.
- Develop against copied sample files.
- Never use a pathname as an asset's identity.
- Do not equate whole-file hashes with recording identity.
- Keep user-state writes and synchronization bookkeeping transactional.
- Make retries idempotent through durable event identity.
- Do not use device wall-clock timestamps as authoritative ordering.
- Record conflict and recovery rules in REQUIREMENTS.md before code
  that depends on them.
- Surface failures; do not swallow them or report incomplete work
  as successful.
- Do not expose secrets, credentials, or authentication tokens in logs.
- Do not reset databases or perform destructive migrations without
  explicit authorization.

## Code and validation

- Keep packages and configuration minimal.
- Prefer explicit control flow over cleverness.
- Handle errors and resource cleanup.
- Use parameterized SQL.
- Keep database schema changes explicit and reproducible.
- Test meaningful invariants and failure cases.
- For the identity milestone, test rescans, renames, duplicate bytes,
  missing files, and changed bytes.
- For sync, test duplicate delivery, lost acknowledgements,
  restarts, concurrency, bootstrap, and recovery.
- Use deterministic failure injection rather than timing-based sleeps.
- Run gofmt, go test ./..., and go vet ./... when applicable.
- Report checks actually run and any limitations. Never invent results.

## First assignment

Implement Milestone 1 from REQUIREMENTS.md.

Inspect the repository first. If empty, create the smallest useful
Go module, SQLite schema, inventory command, and relevant tests.

Choose a maintained SQLite driver appropriate for the Linux VM and
briefly explain any build requirements.

Do not implement playback, HTTP APIs, authentication, or synchronization
in this milestone.

Finish in the reply, not in a new file:
- What works.
- How to run it against a sample directory.
- Checks performed.
- Remaining identity limitations.

Keep the final explanation concise.