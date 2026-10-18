# Public release readiness (local draft, 2026-10-18)

This is a preparation record, not approval to publish. Do not create a new
repository, push a branch or tag, publish a GitHub Release, or replace an
installed copy until the maintainer separately confirms the exact target.

## Scope and ownership

- CodexWorkResumer is a fork of `sybxxx/codex-auto-retry`. Keep the upstream
  Git relationship and original MIT attribution. Fork changes are also under
  MIT; see `LICENSE` and `THIRD_PARTY_NOTICES.md`.
- A public `waterisme/CodexWorkResumer` fork already exists. Before any
  publication, decide whether to update that fork or use another target.
- The fork's already-public `codex/codexworkresumer` branch contains an old
  personal drive path in `docs/architecture-analysis.md`. A clean new commit
  cannot retract that branch or its public history; handle it separately with
  maintainer approval. Do not force-push or delete it during preparation.
- The local development branch has unpublished commits with older personal
  paths and screenshots in its history. **Do not push its history verbatim.**
  Prepare a clean publication commit from the agreed public base, preserving
  upstream attribution, and inspect the resulting diff and history before push.

## Candidate public source set

`README.md`, `README_zh-TW.md`, `LICENSE`, `THIRD_PARTY_NOTICES.md`,
`THIRD_PARTY_LICENSES.txt`,
`CHANGELOG.md`, `VERSION.json`, `.gitignore`, `.gitattributes`, `.mcp.json`,
`.codex-plugin/`, `.github/`, `CONTRIBUTING.md`, `SECURITY.md`,
`CODE_OF_CONDUCT.md`, `assets/README.md`, Markdown files under `docs/`,
`release/`, `scripts/`, and `skills/`.

Do not publish old `assets/*.png`, `docs/*.pptx`, local handoff notes, runtime
config/state/logs, session records, credentials, `node_modules`, or local
build/test caches. The release ZIP excludes `assets/` and `docs/` entirely;
source files are still subject to a separate Git-content review.

## Release gate

1. Verify the exact Git tree and full proposed history for tokens, passwords,
   private paths, task titles, session identifiers, and personal records.
2. Re-check the generated license bundle against locked dependencies. The
   mixed MCP licensing and build-only MPL-2.0 packages need final legal review;
   retain upstream notices.
3. Run version check, Go tests/vet, panel typecheck/build, smoke tests,
   release ZIP verification, and archive-content scan on the final commit.
4. On a clean Windows installation, test install, ordinary Codex connection,
   safe launch, quota read, a real rate-limit wait/resume, temporary-failure
   rescue, Session list behavior, and uninstall with/without retained data.
5. Resolve or explicitly document the reported status-label issues: a
   non-exhausted quota shown as waiting for reset, and an unselected/inactive
   Session shown as waiting for automatic resume. No core behavior was changed
   during this publication-preparation pass.
6. Obtain the maintainer's explicit approval for target repo, clean history,
   release tag, ZIP/hash, and public announcement before any network write.

Static scans and automated tests cannot prove that all live Codex behaviors
work correctly. Until gates 1-6 are complete, this is a local candidate only.

## Local validation snapshot (2026-10-18)

- `verify-version.ps1`, panel typecheck/build, Go test **compilation**, `go vet`,
  the MCP smoke test, and the 40-row Session-list smoke test passed.
- The full Go test run did **not** pass in the restricted environment: nine
  tests that access Windows user environment/registry failed with Access
  Denied, and TestMain cleanup also failed. Running with no selected tests
  still encounters that cleanup restriction. This is not a product-behavior
  verdict; the complete suite needs a safe, authorized Windows test context.
- `release-test.ps1` reached its startup-manager smoke check but stopped on
  Windows registry access denial. Its full installer/uninstaller dry run and
  remaining checks are therefore unverified for Build 15.
- The Build 15 ZIP was regenerated after the licensing update. Read-only
  inspection found 193 file entries, exactly one third-party license bundle,
  no old images/slides or runtime records, and an outer SHA-256 matching the
  companion hash file. The bundle check passed for the Go runtime, 8 Go
  modules, and 96 npm packages.
- No installed application was replaced. No commit was pushed, tag created,
  or GitHub Release published during this preparation pass.
