---
description: Check GitHub's official actions.lock without rewriting registered workflows to inline SHAs. Use for opt-in lockfile verification, offline coverage checks, mixed repositories, unsupported update/options, and required gh tooling.
---

# GitHub workflow dependency lockfiles

`pinact run --actions-lock` accepts GitHub's official
`.github/workflows/actions.lock` for **registered workflows only**. It does not
introduce a pinact lockfile and does not handle Agentic Workflows' `*.lock.yml`.
Without the flag, pinact's existing inline SHA behavior is unchanged.

## Before opting in

Run from the repository root. Enable this flag only after confirming that
GitHub enforces workflow dependency lockfiles for this repository. The public
preview is documented for GitHub Enterprise Cloud. A file on disk does not
prove eligibility or runtime enforcement; pinact cannot verify that entitlement,
especially offline. Keep inline SHA checking enabled on unsupported platforms.
The flag is an explicit assertion of that prerequisite, not auto-detection.

This initial integration supports GitHub.com and lockfile schemas `v0.0.2` and
`v0.0.3`. Older, future and malformed formats fail rather than silently falling
back. GitHub Enterprise Server and data-residency tenants are not supported by
this integration, even though the official extension supports data-residency
hosts. Known incompatible `GITHUB_SERVER_URL`, `GH_HOST`, `GITHUB_API_URL`/`GHES_API_URL`, and `ghes` settings
are rejected when a registered workflow is selected.

## Usage

Install [GitHub CLI](https://cli.github.com/) and its official extension yourself
for online verification:

```sh
gh extension install github/gh-actions-lock
# Configure gh authentication for github.com separately, if needed.
# Generate/onboard with the official tool, then review its complete diff:
gh actions-lock

# Read-only online validation of registered and ordinary workflows:
pinact run --actions-lock --check

# Offline local consistency check; does not require gh or credentials:
pinact run --actions-lock --fix=false --no-api

# Verify one workflow or the workflows registered in the lockfile:
pinact run --actions-lock --check .github/workflows/ci.yml
pinact run --actions-lock --check .github/workflows/actions.lock
```

Use an extension supporting `--verify`, `--no-fix`, `--no-interactive` and
`--hostname` and `--json=valid,findings,workflows`. The implementation was checked against
[gh-actions-lock at d5dca22](https://github.com/github/gh-actions-lock/tree/d5dca22a06ff8590d50b41612038996800cfb336).
The extension is pre-1.0: pin/update it deliberately and review behavior changes.
Missing tools, authentication errors, unsupported flags and failed verification
are errors. pinact never installs a tool or changes authentication settings.
`PINACT_GITHUB_TOKEN`/pinact's keyring are not forwarded as gh credentials; gh
uses its own supported credentials, including `GH_TOKEN`/`GITHUB_TOKEN`.

## What is checked

| Mode | Guarantee | Not guaranteed |
| --- | --- | --- |
| `--actions-lock --fix=false --no-api` | Official parser validates schema, digest syntax, identity-field structure and graph cycles; actual YAML `jobs`/`steps` references match the selected workflow's registration; every reachable recorded dependency exists; stale direct pins fail | Upstream repository identity, commit/ref authenticity, remote action contents, missing edges in a forged graph, runtime eligibility/enforcement |
| `--actions-lock --check` (online) | Local checks above, then the official `gh actions-lock --verify --no-fix` frozen verification, scoped to selected registered workflows | Missing edges in a forged remote composite graph, `owner_id` equality, another repository's reusable workflow dependencies, runtime eligibility/enforcement |

Offline success is reported as **local coverage/recorded graph checking**, with
an explicit warning about unverified upstream identity, refs and completeness.
There is no GitHub API or external CLI call in this path. Unregistered files
continue to use pinact's ordinary [offline check](check.md).

Online verification delegates repository identity, upstream reachability and moved
refs to the official CLI. The investigated verifier enforces the recorded
`repo_id`; redirects to the same repository ID (including rename/transfer) are
warnings in the official CLI, not a guarantee that `owner_id` is unchanged.
pinact requests a structured report and requires every selected workflow to be
verified, `valid: true`, and **zero findings**. Unlike merely trusting its exit
code, this rejects warnings too: inconclusive reachability, same-ID redirects,
and the SHA-as-ref warning (whose path may skip upstream parity). Those failures
never change files. Offline coverage can still accept an inline SHA; it is never
rewritten to make online verification pass. Full-version tags must still point to the locked
commit; branch/partial-version pins are history-checked without advancing them.
Suspicious changes are not accepted automatically. This is verification of the
**recorded pins**, not an independent reconstruction of upstream action.yml
files: the official frozen verifier may trust an already-recorded composite
graph, so an omitted graph edge is not proven absent upstream. Do not describe
online success as an independent graph-completeness audit. The extension's
`GH_ACTIONS_LOCK_*` overrides are removed so they cannot redirect verification
to a different lockfile, proxy or insecure transport. The repository root and
GitHub.com host are set explicitly. gh update notifications are disabled for
this invocation. The extension may write its own diagnostic cache/run logs;
workflow and lockfile contents remain read-only.

Pin keys are repository-and-ref scoped, **not subpath scoped**. For example,
`actions/cache/save@v4` and `actions/cache/restore@v4` share the same downloaded
repository tree and lockfile pin; `@v3` cannot satisfy `@v4`. Owner/repository
case is normalized; ref case is preserved. Existing inline SHAs can match the
recorded commit and are never converted back to tags.

Recorded composite dependencies are followed through `dependencies[].uses`.
GitHub's documentation describes caller-side pinning of a remote reusable
workflow and callee-side locking of its internal dependencies. However, the
investigated official CLI only extracts action references:
[ExtractActionRefs](https://github.com/github/gh-actions-lock/blob/d5dca22a06ff8590d50b41612038996800cfb336/internal/workflowfile/workflowfile.go)
uses the SDK's `ParseActionRef`, which excludes reusable-workflow files.
Therefore **remote reusable workflow calls fail explicitly in this integration**,
even if someone inserts a caller pin by hand. Their authentication cannot be
claimed through that verifier. Pin their caller references inline and verify
callee repositories separately. Same-repository reusable workflow calls retain
their own workflow registration context, rather than inheriting the caller's
list of action pins.
README examples and composite action files never gain workflow coverage merely
because the lockfile lists a similar name.

## Fix, update and selection

A normal `pinact run --actions-lock` verifies registered workflows without
changing their references or any locked commit. It still pins ordinary files.
All selected registered workflows are validated **before any file is written**,
so their validation failure cannot leave a partially pinned mixed invocation.
This does not change pinact's existing per-file failure semantics for ordinary
files after the lockfile preflight succeeds.

`--check`/`--fix=false` change neither workflows nor the lockfile. File arguments
and configuration `files` patterns select workflows normally. Explicitly naming
`actions.lock` expands to its registered workflow files. Unregistered workflows
retain inline SHA pinning, verification and update behavior.

When registered workflows are selected, these combinations fail before writes:

- `--update`: latest-version selection is not the same as `gh actions-lock
  --relock`. Use Dependabot, or edit workflow versions and review regeneration
  with the official tool. pinact does not generate, onboard, repair or update
  lockfiles in this first implementation.
- `--verify-comment`, `--verify-min-age`, `--min-age`, `--branch-to-tag`, action
  `--include`/`--exclude`, `--format`, and configuration `ghes`, `min_age`,
  `rules`/`ignore_actions`: their guarantees cannot be silently dropped or
  translated to the lockfile model. Select ordinary files separately with an
  appropriate configuration; use `gh actions-lock --verify` for locked files.
- Remote reusable workflow calls, step-level `./…`/`$/…` actions and `docker://…`: this first integration does
  not scan in-repository composite trees or validate container digests. Use the
  official verifier/a separate container policy; these references are not
  silently accepted as covered.

`--actions-lock --diff-file` is rejected even if the diff has no selected
workflow lines: a lockfile-only change needs full checkout context. Run a full
check without `--diff-file`. No filtering can suppress a selected registered
workflow's dependency validation.

Invalid coverage or an unsupported operation exits 2. Ordinary pinact exit
codes remain as documented in [Exit codes](exit_codes.md).

## Specification sources

The preview was investigated on 2026-10-13:

- [GitHub concepts](https://docs.github.com/en/actions/concepts/security/workflow-dependency-lockfiles),
  [generation](https://docs.github.com/en/actions/how-tos/secure-your-work/lock-workflow-dependencies),
  and [enforcement](https://docs.github.com/en/actions/how-tos/administer/enforce-workflow-lockfiles).
- [Official Go parser v0.0.6](https://github.com/github/actions-lockfile/tree/31f0e0ba8475fd07d210a1a0f142dd101db4f162/go/pkg/lockfile):
  schema versions, repo/ref pin keys, prefixed commits, numeric identity and
  graph validation. The parser normalizes accepted formats in memory only.
- [Official verification implementation](https://github.com/github/gh-actions-lock/blob/d5dca22a06ff8590d50b41612038996800cfb336/cmd/gh-actions-lock/verify.go)
  and [recorded dependency graph](https://github.com/github/gh-actions-lock/blob/d5dca22a06ff8590d50b41612038996800cfb336/internal/lockfile/state.go).

GitHub's concepts-page example currently shows `v0.0.2`; the current official
producer/parser also handles `v0.0.3`. Consult both, rather than inferring the
schema from that example alone. Require-lockfile policy can block unregistered
workflows at runtime; accepting a registered workflow here does not establish
that such a policy is enabled for the repository.
