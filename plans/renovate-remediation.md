# Renovate remediation

Renovate (Mend-hosted `renovate[bot]`, onboarded 2026-04-15 via issue #21)
has **never** opened a pull request against this repository, and separately
reports a lookup failure for `ko-build/setup-ko`. This document records what
was established and what has to change.

Two independent problems. Fixing the `ko` pin does **not** fix PR creation,
and vice versa.

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
Renovate resolved the new *version* fine — the dashboard correctly offers
`v0.10` — but resolving a digest for the current value meant looking up a
`v0.9.0` ref that does not exist, so the update could never be built.

Fixed by pinning the real `v0.10` tag, which also makes the comment name a
tag that exists:

```yaml
uses: ko-build/setup-ko@61b4d1d396f5b2e7d6bb6fefdce3dc38d1a13445 # v0.10
```

Both tags are lightweight and point directly at commits, verified with
`git ls-remote https://github.com/ko-build/setup-ko 'refs/tags/v0.10' 'refs/tags/v0.10^{}'`.

Guard against a repeat: the version comment must name a tag upstream actually
publishes. `actions/checkout # v6` and `actions/setup-go # v6.3.0` are both
fine — Renovate resolves digest updates for them today.

## Problem 2: no PRs at all — the ruleset rejects branch creation

### Root cause

Repository ruleset **"basic protection"** (id 14480935, active, created
2026-03-29, last changed 2026-03-30) targets `~ALL` refs, excluding only
`refs/heads/users/**/*` and `refs/heads/Claude/**/*`. One of its rules is:

```json
{
  "type": "required_status_checks",
  "parameters": {
    "do_not_enforce_on_create": false,
    "required_status_checks": [{ "context": "streamloom", "integration_id": 15368 }]
  }
}
```

`do_not_enforce_on_create: false` means the `streamloom` check is enforced on
**ref creation**, not just on merge. A branch being created carries a brand-new
commit that CI has never seen, so the check can never already be passing, so
the creation is rejected. Because the ruleset targets `~ALL`, this applies to
every `renovate/*` branch.

This was observed directly. Pushing this very branch produced:

```
remote: Bypassed rule violations for refs/heads/claude/renovate-pr-failures-uddt4l:
remote: - Required status check "streamloom" is expected.
```

The push was a rule violation and succeeded only because the Claude GitHub App
holds a bypass. `renovate[bot]` has no such bypass, so the identical push is
rejected outright.

A second push, *updating* that branch rather than creating it, reported both
rules:

```
remote: - Changes must be made through a pull request.
remote: - Required status check "streamloom" is expected.
```

So the two rules block different stages: `required_status_checks` rejects the
initial creation, and `pull_request` additionally rejects every later push to
the branch. Even if creation were permitted, Renovate could never push a rebase
or a follow-up version bump onto its own branch.

Note the ruleset's `bypass_actors` comes back absent from the REST API even
though a bypass demonstrably occurred, so that field cannot be trusted to
enumerate who is exempt here.

### Why this matches every symptom

- The ruleset (2026-03-30) predates Renovate's onboarding (2026-04-15), so
  Renovate has been blocked since its very first run. This is not a
  regression — it never worked here.
- No `renovate/*` ref has ever existed. `git ls-remote origin` returns 5
  branches and `refs/pull/1…44`; PRs #1–#44 are all human/agent-authored.
- It hits all ten updates uniformly, regardless of manager — `github-actions`,
  `gomod`, `devbox`, and lock-file maintenance alike. No file-type-specific
  theory explains that spread; a rule on `~ALL` refs does.
- `claude/*` and `dskiff/*` branches exist under the identical four rules only
  because those pushes were bypassed, as shown above.

Renovate is otherwise healthy: dashboard issue #21 has 0 comments and 0
timeline events but its `updated_at` moves, so the bot still runs and rewrites
the body each pass. That also rules out `dryRun`, which skips `ensureIssue`.

### Why it shows as "pending" rather than an error

All ten updates sit under `## Other Branches` ("The following updates are
pending. To force the creation of a PR, click on a checkbox below."). In
`dependency-dashboard.ts` that bucket is everything *not* in `otherRes`
carrying no PR number; with no branch and no PR the only reachable result is
`no-work`, returned from `branch/index.ts` at `if (!commitSha && !branchExists)`.
So `commitFilesToBranch()` returned `null` for every update.

The dashboard shows no `## Errored` and no `## Repository Problems` section,
so nothing threw and nothing logged at `warn`/`error`. In `util/git/error.ts`
the rejection path that stays quiet is:

```ts
if ((err.message.includes('remote rejected') || err.message.includes('403')) &&
    files?.some((file) => file.path?.startsWith('.github/workflows/'))) {
  logger.info('Workflows update rejection - aborting branch.');
  return null;
}
```

A ruleset rejection message contains `remote rejected`, so the five GitHub
Actions updates land here and abort silently. Current Renovate `main` also has
an explicit `GH013` branch that *throws* a config-validation error, and the
non-workflow branches would fall through to `throw err` — neither of which is
visible on the dashboard. The likeliest reading is that the Mend-hosted build
predates the `GH013` handler and swallows these differently; the run log at
<https://developer.mend.io/github/dskiff/streamloom> would confirm. This is a
reporting detail, not a separate cause — the blocked push is established
independently by the bypass notice above.

### Remediation

Pick one; the first is the real fix.

1. **Scope the ruleset to the default branch.** `deletion`,
   `non_fast_forward`, `pull_request` and `required_status_checks` are all
   rules for protecting `main`. Change the target from `~ALL` to
   `~DEFAULT_BRANCH` (or `refs/heads/main`) and the exclusion list becomes
   unnecessary.
2. Tick **"Do not require status checks on creation"**
   (`do_not_enforce_on_create: true`). **Not sufficient alone** — it unblocks
   the initial creation, but the `pull_request` rule still rejects every
   subsequent push to the branch, so Renovate could not rebase or re-bump it.
3. Add `renovate[bot]` as a bypass actor.
4. Add `refs/heads/renovate/**/*` to the exclusion list — but see the
   case-sensitivity note below before relying on exclusions.

After this, confirm `renovate/*` branches and PRs actually appear; if the
GitHub Actions updates alone stay stuck, check that the Renovate GitHub App
has **Workflows: Read and write**, which is required to push any branch
touching `.github/workflows/`.

## Problem 3: automerge cannot succeed either (latent, next blocker)

`renovate.json` sets `automerge: true` for `minor`/`patch`/`pin`/`digest` —
effectively every update this repo sees. Even once branches can be created it
will not work as configured.

Effective rules, verified with `GET /repos/dskiff/streamloom/rules/branches/{branch}`:

| Branch | Rules applied |
| --- | --- |
| `renovate/go-chi-chi-5.x` | deletion, non_fast_forward, pull_request, required_status_checks |
| `claude/renovate-pr-failures-uddt4l` | deletion, non_fast_forward, pull_request, required_status_checks |
| `Claude/foo` | none |
| `users/dskiff/foo` | none |

- The `pull_request` rule requires **1 approving review**. Renovate cannot
  approve its own PR, so every automerge stalls awaiting a human. Either drop
  `automerge`, give `renovate[bot]` a bypass, or accept manual approval.
- `non_fast_forward` blocks force pushes on `renovate/*`. Renovate pushes with
  `--force-with-lease` (`pushCommit` in `util/git/index.ts`), which is fine for
  creating a branch but will be rejected the moment it rebases one onto a
  moved `main`.

Scoping the ruleset to `main` (remediation 1 above) resolves all of this at
once.

### Also worth fixing: the exclusions are case-sensitive

`refs/heads/Claude/**/*` does not match the lowercase `claude/...` branches
this repo actually gets — confirmed in the table above, where `claude/…`
receives all four rules while `Claude/foo` receives none. The exemption is a
no-op as written, which is why pushing this branch needed an app bypass at
all. If exclusions are kept, spell them lowercase.

## Note on CI coverage

`ci.yml` runs on `push` to `main` and on `pull_request`, so the required
`streamloom` check does run on a Renovate PR once one exists. It does not run
on a bare branch push, which is precisely why enforcing that check at creation
time can never be satisfied.
