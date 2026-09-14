# Renovate remediation

Renovate (Mend-hosted `renovate[bot]`, onboarded 2026-04-15 via issue #21)
has **never** opened a pull request against this repository, and separately
reports a lookup failure for `ko-build/setup-ko`. This document records what
was established, what remains open, and what has to change.

Two independent problems. Fixing the `ko` pin does **not** fix PR creation.

## Problem 1: `ko-build/setup-ko` digest lookup — root-caused, fixed here

Dependency Dashboard warning:

> Renovate failed to look up the following dependencies:
> `Could not determine new digest for update (github-tags package ko-build/setup-ko)`.
> Files affected: `.github/workflows/publish.yml`

`publish.yml` pinned:

```yaml
uses: ko-build/setup-ko@d006021bd0c28d1ce33a07e7943d48b079944c8d # v0.9.0
```

The upstream tag list is `v0.1` … `v0.9`, `v0.10` — two-component tags only.
**There is no `v0.9.0` tag.** The pinned digest `d006021…` is in fact
`refs/tags/v0.9`; only the trailing version comment was wrong.

For a `uses: owner/repo@<sha> # <version>` line the `github-actions` manager
takes the SHA as `currentDigest` and the **comment** as `currentValue`.
Renovate could resolve the new *version* (the dashboard correctly offers
`v0.10`) but resolving a digest for the current value meant looking up a
`v0.9.0` ref that does not exist, so the update could never be built.

Fixed by pinning the real `v0.10` tag, which also makes the comment name a
tag that exists:

```yaml
uses: ko-build/setup-ko@61b4d1d396f5b2e7d6bb6fefdce3dc38d1a13445 # v0.10
```

Both tags are lightweight and point directly at commits, verified with
`git ls-remote https://github.com/ko-build/setup-ko 'refs/tags/v0.10' 'refs/tags/v0.10^{}'`.

Guard against a repeat: the version comment must be a tag that upstream
actually publishes. `actions/checkout # v6` and `actions/setup-go # v6.3.0`
are both fine — Renovate resolves digest updates for them today.

## Problem 2: no PRs at all — state established, trigger needs run logs

### What is established

- No `renovate/*` ref has ever existed. `git ls-remote origin` returns 5
  branches and `refs/pull/1…44`; PRs #1–#44 are all human/agent-authored.
  So this is not "Renovate stopped" — it never produced a branch here.
- Renovate is still running. Dashboard issue #21 has 0 comments and 0
  timeline events but `updated_at` moves, so the bot is rewriting the body.
  That also rules out `dryRun`, which skips `ensureIssue` entirely.
- All 10 updates sit under `## Other Branches` ("The following updates are
  pending. To force the creation of a PR, click on a checkbox below.").
  In `dependency-dashboard.ts` that bucket is everything *not* in `otherRes`
  and carrying no PR number. With no branch and no PR the only reachable
  result is `no-work`, returned from `branch/index.ts` at
  `if (!commitSha && !branchExists)`.
- Therefore `commitFilesToBranch()` returns `null` for every update.
- The dashboard shows **no** `## Errored` and **no** `## Repository Problems`
  section, so nothing threw and nothing was logged at `warn`/`error`. That
  excludes every loud failure path in `util/git/error.ts`: the GH013 ruleset
  violation, `protected branch hook declined`, `GH003` force-push denial,
  and the `workflows`-permission path that logs at `warn`.

That leaves only the paths that return `null` quietly:

| Path | Logged at | Applies to |
| --- | --- | --- |
| push rejected (`remote rejected` / `403`) **and** the change touches `.github/workflows/` → "Workflows update rejection - aborting branch." | `info` | the 5 action updates |
| `No files to commit` (`updatedPackageFiles` + `updatedArtifacts` both empty) | `debug` | plausible for lock-file maintenance and `gosec@latest` |
| `No file changes detected. Skipping commit` | `debug` | — |

The 5 GitHub Actions updates match the first row exactly, and that row's
usual cause is the GitHub App lacking **`workflows: write`**, which is
required to push any branch touching `.github/workflows/`. Renovate aborts
those branches silently by design.

`Lock file maintenance` touches no package file — only `go.sum` — so if the
Go toolchain cannot regenerate artifacts there is nothing to commit and the
branch is skipped silently. `gosec@latest` in `devbox.json` has `latest` as
its literal current value, which plausibly leaves nothing to rewrite.

### What is still open

The three `gomod` updates (`chi` → v5.3.2, `slog-chi` → v1.19.1, `testify` →
v1.12.1) are not explained by the table above: a `go.mod` bump always changes
a package file, and a failed artifact run sets `artifactErrors`, forces a PR,
and would surface. Static analysis cannot close this without the run logs.

Check the Mend job log at <https://developer.mend.io/github/dskiff/streamloom>
and search for these lines — they discriminate between the candidates:

- `Workflows update rejection - aborting branch.` → missing `workflows: write`
- `No files to commit` / `No file changes detected` → nothing was generated
- any `remote rejected` / `403` on a non-workflow branch → repo-wide write denial

### Remediation

1. Re-check the Renovate GitHub App installation on `dskiff/streamloom` and
   grant **Workflows: Read and write** (plus confirm Contents: Read and
   write). Accept any pending permission request — the installation keeps
   working for issues while workflow pushes fail, which is exactly the
   observed split.
2. Pull the Mend run log and match against the table above for the `gomod`
   branches before changing anything else.

## Problem 3: automerge can never succeed (latent, blocks the next step)

`renovate.json` sets `automerge: true` for `minor`/`patch`/`pin`/`digest` —
effectively every update this repo sees. It cannot work as configured.

Repository ruleset "basic protection" (id 14480935, active, last changed
2026-03-30) targets `~ALL` refs and excludes only `refs/heads/users/**/*` and
`refs/heads/Claude/**/*`. Verified per-branch with
`GET /repos/dskiff/streamloom/rules/branches/{branch}`:

| Branch | Rules applied |
| --- | --- |
| `renovate/go-chi-chi-5.x` | deletion, non_fast_forward, pull_request, required_status_checks |
| `claude/renovate-pr-failures-uddt4l` | deletion, non_fast_forward, pull_request, required_status_checks |
| `Claude/foo` | none |
| `users/dskiff/foo` | none |

Two consequences:

- The `pull_request` rule requires **1 approving review** and lists no bypass
  actors. Renovate cannot approve its own PR, so every automerge stalls
  awaiting a human. Either drop `automerge`, or add `renovate[bot]` as a
  bypass actor, or accept that every update needs a manual approval.
- `non_fast_forward` blocks force pushes on `renovate/*`. Renovate pushes
  with `--force-with-lease` (`pushCommit` in `util/git/index.ts`), which is
  fine for creating a branch but will be rejected the moment it rebases one
  onto a moved `main` — a failure that will appear only after PR creation
  starts working.

The ruleset does **not** block branch creation: `claude/*` branches are
covered by the identical four rules and were created after the ruleset's last
edit. So the ruleset is not the cause of Problem 2.

### Also worth fixing: the exclude is case-sensitive

`refs/heads/Claude/**/*` does not match the lowercase `claude/...` branches
this repo actually gets — confirmed above, `claude/…` receives all four rules
while `Claude/foo` receives none. The exclusion is a no-op as written. If the
intent is to exempt agent branches, use `refs/heads/claude/**/*`, and add
`refs/heads/renovate/**/*` with the same lowercase care.

## Note on CI coverage

`ci.yml` runs on `push` to `main` and on `pull_request`, so the required
`streamloom` check does run on a Renovate PR once one exists. No change
needed — but note it does not run on a bare branch push, so the branch status
stays pending until the PR is opened.
