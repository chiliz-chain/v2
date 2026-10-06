---
applyTo: "version/version.go,params/version.go"
---

# Reviewing version changes

Chiliz versions independently of upstream: this client is `2.x.y`, unrelated to BSC's `1.x.y` tags.

- **Never take upstream's numbers.** Upstream moved the canonical constants to `version/version.go`
  in v1.7.x, so every sync tag conflicts here. `params/version.go` still carries mirrored
  `VersionMajor` / `VersionMinor` / `VersionPatch` constants.
- **Both files must agree.** A diff that changes one and not the other is a finding.
- **Any release-bound PR must bump the patch component** if the current compiled-in version already
  has a published release tag. Releases are tagged bare `X.Y.Z`, the release workflow names the
  release from the git tag, and the binary self-reports from these files — the tag must equal the
  compiled-in version. PR #65 shipped without the bump and needed a last-minute manual commit.
- Upstream syncs get this bump automatically from `.github/scripts/bsc-sync/run-sync.sh`, which
  reports the outcome in the PR body. Hand-written release PRs do not.

**Scope note.** This file only loads when one of the two version files is already in the diff, so it
catches *mismatches*, never *omissions*. The release-bound PR that touches neither file — the PR #65
case — can only be caught by the always-on rule in `.github/copilot-instructions.md`.
