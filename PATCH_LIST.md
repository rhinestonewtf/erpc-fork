# Fork patch list

Every change this fork carries on top of upstream `erpc/erpc`. **This file is the
source of truth for what "our patches" are.**

Workflow: the patch itself goes in through a PR and is squash-merged; its row here is
then committed **directly to `main`**, because the squash hash does not exist until
after the merge. If you drop a patch, move it to [Removed](#removed-patches) with the
reason.

Currently rebased onto upstream **`0.1.2`** (`803b67d8`, released 2026-08-05).

## Why this file exists

`main` is maintained by rebasing our commits onto an upstream tag and force-pushing
(`sync-with-upstream.sh`, `git push --force-with-lease`). That works, but it means:

- **Commit hashes below are rewritten by every sync.** They are a snapshot for
  archaeology, not an identity. Verify a patch is present with its **probe**, never
  with its hash.
- **A patch can vanish silently.** It has happened once — see
  [Removed](#removed-patches). A `reset` + cherry-pick reconstruction of the patch set
  dropped a merged code fix two days after it landed, and nobody noticed for six months,
  because the working mental model was "our patches are the CI and config ones".

That last point is why `90800261` deserves a warning: it is named *"update prod
config"* but also carries two Go code patches. Do not assume a commit's subject tells
you whether it touches code.

## Verifying after a sync

```bash
./sync-with-upstream.sh <tag>     # rebases + force-pushes main
git log --oneline upstream/main..main   # expect exactly the Active patches below
```

Then run every probe in the table. All of them must print `OK`:

```bash
grep -q "isTerminalGasLimitRejection" architecture/evm/error_normalizer.go && echo OK
grep -q "terminalGasLimitRejections" architecture/evm/gas_limit_errors.go && echo OK
grep -q 'strconv.ParseUint(s, 10, 64)' common/utils.go && echo OK
grep -q '"wss://"' common/defaults.go && echo OK
grep -q "pnpm@" Dockerfile && echo OK
test -f erpc-prod.yaml && echo OK
test -f buildspec-amd64.yml && echo OK
test -f buildspec-arm64.yml && echo OK
test -f .github/workflows/docker.yaml && echo OK
test -f promote-to-prod.sh && echo OK
test -f PATCH_LIST.md && echo OK
grep -q "This is a fork" CLAUDE.md && echo OK
```

A rebase that reports success while a probe fails means the patch was dropped during
conflict resolution. Recover it from the pre-rebase `main` in the reflog
(`git reflog main`), not from a rewritten branch.

Go patches also need `go test ./architecture/evm/... ./common/...` to pass — a patch
can survive a rebase textually and still be defeated by upstream logic that moved
around it.

## Active patches

### Code

| Patch | Files | Probe |
| -- | -- | -- |
| `e4494947` fix(evm): treat gas-limit rejections as terminal, not retryable (RHI-6277, #6) | `architecture/evm/gas_limit_errors.go`, `architecture/evm/gas_limit_errors_test.go`, `architecture/evm/error_normalizer.go` | `isTerminalGasLimitRejection` present in `error_normalizer.go` |
| `90800261` (part of "update prod config") base-10 EVM quantity tolerance | `common/utils.go` | `strconv.ParseUint(s, 10, 64)` in `common/utils.go` |
| `90800261` (part of "update prod config") treat `ws://`/`wss://` endpoints as providers | `common/defaults.go` | `"wss://"` in `common/defaults.go` |
| `8abdf895` fix(docker): pin pnpm to the packageManager version (#7) | `Dockerfile` | `pnpm@` in `Dockerfile` (i.e. a version, not bare `pnpm`) |

**RHI-6277 — gas-limit rejections are terminal.** A rejection of the transaction's gas
limit is classified `ErrEndpointExecutionException` and **not** marked retryable toward
other upstreams. The per-transaction cap (EIP-7825, 2^24 on most chains) and the
intrinsic-gas floor are consensus rules, and the limit is fixed in the signed bytes, so
no failover and no fee bump changes the answer. Upstream instead routes these through
the generic `-32003` branch, which marks `eth_sendRawTransaction` retryable on the
rationale that providers differ in gas estimation — true for pool admission, false for a
consensus bound. Cost of the upstream behaviour, measured: a Base 8453 lane wedged 8h
because a ~100ms `gas limit too high` became ~17s of `ErrUpstreamsExhausted`, past the
relayer's 5s submit timeout, so it re-broadcast permanently-invalid bytes 686 times.

*Upstreamable:* yes, and worth doing — the carve-out is deliberately narrow and does not
contradict upstream #1094. If it lands upstream, delete this patch rather than carrying it.

*Rebase risk:* low. The logic lives in its own file; only the five-line call site in
`error_normalizer.go` can conflict. It **must stay above** the `-32003` /
`"out of gas"` branch — if a rebase moves it below, the tests fail rather than
silently regressing.

**pnpm pin.** `Dockerfile` installed pnpm unpinned (`npm install -g pnpm`), so the build
took whatever version was newest that day. pnpm 11 verifies the package-manager identity
against the lockfile and aborts with `ERR_PNPM_PNPM_ENGINE_IDENTITY_UNVERIFIABLE`, since
`@pnpm/exe.<platform>` is absent from a `lockfileVersion: 9` file written by pnpm 10 — so
every image build broke on a repo where nothing had changed. Now pinned to the version
`package.json` already declares as `packageManager`; **the two must stay in step.** Also
drops the duplicate install in `ts-dev`, which derives `FROM ts-core`.

*Upstreamable:* yes, and it should go up — upstream `main` has the same unpinned install
and the fix has no fork-specific content. Every other base image here is digest-pinned.

*Rebase risk:* low, but note the probe only checks that a version is pinned at all. If
upstream bumps `packageManager` in `package.json`, the pin must be bumped with it or
`--frozen-lockfile` will fail on a version mismatch instead.

**Base-10 quantity tolerance.** Some upstreams return EVM quantities as base-10 strings
instead of `0x`-prefixed hex, which broke upstream health tracking. `HexToUint64` /
`HexToInt64` accept both.

*Upstreamable:* probably. Never proposed.

**`ws://` / `wss://` endpoints.** `convertUpstreamToProvider` treated websocket endpoints
as needing provider conversion; they don't.

*Upstreamable:* probably. Never proposed.

### Config and CI

| Patch | What |
| -- | -- |
| `90800261` update prod config | `erpc-prod.yaml` — the production network/upstream config |
| `d620ce1f` Cleanup workflows | deletes upstream's CI (benchmark, codeql, dependency-review, release, scorecards, test) |
| `decddca5` Setup minimal CI | our own minimal workflow set |
| `3c8bdf46` Add promote-to-prod script | `promote-to-prod.sh`, `release.sh`, `sync-with-upstream.sh` |
| `a75101a2` feat(ci): CodeBuild multi-arch images (RHI-5507, #4) | `buildspec-amd64.yml`, `buildspec-arm64.yml`, `.github/workflows/docker.yaml` |
| `31a060e5` ci: `workflow_dispatch` trigger (RHI-5509, #5) | `.github/workflows/docker.yaml` |

### This file

| Patch | Files | Probe |
| -- | -- | -- |
| `e4494947` docs: the patch register itself (#6) | `PATCH_LIST.md`, `CLAUDE.md` | `test -f PATCH_LIST.md`, and `grep -q "This is a fork" CLAUDE.md` |

Listed because it is a fork-only file and can be dropped by a sync like any other patch —
and losing it loses the ability to detect that anything else was dropped. `CLAUDE.md` is
upstream's file with our section appended, so a rebase may conflict there: keep the
appended section and take upstream's version of the rest.

## Removed patches

**`29e35fc7` fix: make near-head `ErrEndpointMissingData` retryable towards upstream**
(fork PR #2, merged 2026-02-23, dropped 2026-02-25, formally removed 2026-08-28).

Marked a `block not found` / unexpected-empty error retryable toward the *same* upstream
when the requested block was 1–4 blocks ahead of that upstream's tracked head, on the
grounds that it was a propagation race rather than missing data. Without it,
`ErrEndpointMissingData` was permanently non-retryable, both upstreams were marked
consumed, and the network retry died with *"no upstreams left to select"* when ~500ms
would have sufficed.

*How it was lost:* not an upstream conflict. On 2026-02-25 `main` was `reset` to upstream
tag `0.0.62` and the patch set rebuilt with three hand-picked cherry-picks — all CI and
config. This code fix, merged 48 hours earlier, was not among them. Later syncs faithfully
carried forward a `main` that no longer had it.

*Why it is not being restored:* upstream `0.1.2` covers the same ground, better.
`6a0221d4` (upstream #766) removed `ErrCodeEndpointMissingData` from the no-retry list in
`IsRetryableTowardsUpstream`, so it is retryable by default; `network_executor.computeDelay`
now applies a block-time-aware delay (EMA block time × `blockUnavailableDelayMultiplier`)
for exactly `ErrUpstreamBlockUnavailable`, `ErrEndpointMissingData` and emptyish results;
and `emptyResultBeyondConfidence` returns the truthful empty rather than manufacturing
missing-data past the confidence head. Restoring the patch would fight all three, and its
4-block window and regex block-number scrape were cruder than the block-time estimate.

Note the timing: upstream #766 landed 2026-02-18, five days *before* this patch was
written, but was not in the `0.0.62` tag we were pinned to. The patch was correct for the
base we were on.
