# GitHub Release draft — v1.3.1 Build 15 (NOT PUBLISHED)

CodexWorkResumer for Windows x64 monitors Codex quota and can resume the
same interrupted Codex task after supported recoverable failures. It is based
on the MIT-licensed `sybxxx/codex-auto-retry` project; original attribution
and third-party notices and license texts are included.

## Highlights

- 5H and weekly remaining quota, reset countdown, and a Windows tray controller.
- Same-task recovery via the official Codex Desktop IPC when available;
  optional shared-backend and CLI fallback paths remain opt-in.
- Session quota watchlist, manual Resume, ordering, and two queue modes.
- Traditional Chinese and English UI. The separate Mini Monitor is not part
  of this application.
- Build 15 prepares public documentation and a narrower, checked installer
  package. It does not change automatic recovery scheduling or classification.

## Install / remove

Extract the Windows x64 ZIP first. Run `安裝.cmd`; see
`README-安裝說明.txt` for connection conditions and complete instructions.
Run `卸載.cmd` to remove the program while retaining local settings/logs.
The documented `-RemoveData` option removes those local records as well.
End users do not need Go or Node.js.

## Important limits before publication

This text is a draft, not evidence of a tested public release. Real installed
Windows behavior, quota-reset recovery, safe launch and uninstall still need
final acceptance. The two reported status-label issues are tracked in
`docs/PUBLICATION_READINESS.md`. Do not publish this draft or attach a ZIP
until the checklist and maintainer approval are complete.

When approved, attach the exact verified `CodexWorkResumer-v1.3.1-b15-windows-x64.zip`
and matching `.sha256.txt` from the same final source commit. State the tested
commit, Windows version, test results, remaining limitations, and SHA-256.
