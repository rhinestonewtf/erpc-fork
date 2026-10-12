# Fork patch list

Every change this fork carries on top of upstream `erpc/erpc`. **This file is the
source of truth for what "our patches" are.**

Workflow: the patch itself goes in through a PR and is squash-merged; its row here is
then committed **directly to `main`**, because the squash hash does not exist until
after the merge. If you drop a patch, move it to [Removed](#removed-patches) with the
reason.

Currently rebased onto upstream **`0.4.0`** (`120c35e1`, released 2026-10-11).

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

That last point is why `301093f3` deserves a warning: it is named *"update prod
config"* but also carries a Go code patch. Do not assume a commit's subject tells
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
grep -q "resolveRateLimiterRedisTarget" upstream/ratelimiter_registry.go && echo OK
grep -q "common.CorroboratedHead(" health/tracker.go && echo OK
grep -q "common.CorroboratedHead(" erpc/networks.go && echo OK
grep -q "pollFinalizedBlockNumberScheduled" architecture/evm/evm_state_poller.go && echo OK
grep -q "BlockTimestamp" data/connector.go && echo OK
grep -q "latestBlockShared.TryUpdateIfStale(" architecture/evm/evm_state_poller.go && echo OK
test -f erpc-prod.yaml && echo OK
test -f buildspec-amd64.yml && echo OK
test -f buildspec-arm64.yml && echo OK
test -f .github/workflows/docker.yaml && echo OK
test -f sync-with-upstream.sh && echo OK
test -f PATCH_LIST.md && echo OK
grep -q "This is a fork" CLAUDE.md && echo OK
```

A rebase that reports success while a probe fails means the patch was dropped during
conflict resolution. Recover it from the pre-rebase `main` in the reflog
(`git reflog main`), not from a rewritten branch.

Go patches also need `go test ./architecture/evm/... ./common/... ./health/... ./data/...` to pass — a patch
can survive a rebase textually and still be defeated by upstream logic that moved
around it.

## Active patches

### Code

| Patch | Files | Probe |
| -- | -- | -- |
| `23ac203b` fix(evm): treat gas-limit rejections as terminal, not retryable (RHI-6277, #6) | `architecture/evm/gas_limit_errors.go`, `architecture/evm/gas_limit_errors_test.go`, `architecture/evm/error_normalizer.go` | `isTerminalGasLimitRejection` present in `error_normalizer.go` |
| `301093f3` (part of "update prod config") base-10 EVM quantity tolerance | `common/utils.go` | `strconv.ParseUint(s, 10, 64)` in `common/utils.go` |
| `ade471ae` fix(ratelimiter): parse rediss:// URI for the Redis store (RHI-6529, #9) | `upstream/ratelimiter_redis_target.go`, `upstream/ratelimiter_redis_target_test.go`, `upstream/ratelimiter_registry.go` | `resolveRateLimiterRedisTarget` called in `ratelimiter_registry.go` |
| `a503823d` fix(health): serve the corroborated head within lag noise, capped (RHI-8079, #12, #13) | `common/head_noise_tolerance.go` (+ `_test`), call sites in `health/tracker.go` (head + block-time sample) and `erpc/networks.go`; new tests in `health/`, `erpc/`, `internal/policy/`; updated expectations in `health/tracker_*_test.go`, `erpc/networks_test.go`, `erpc/networks_served_tip_test.go`, `erpc/networks_selection_policy_realpoll_test.go`; `docs/pages/reference/evm/block-tracking.mdx` | `common.CorroboratedHead(` called in both `health/tracker.go` and `erpc/networks.go` |
| `61db4a8e` feat(evm): debounce the finalized poll on finality period, not block time (RHI-8113, #14) | `architecture/evm/evm_state_poller_finalized_debounce.go` (+ `_test`), call sites in `architecture/evm/evm_state_poller.go`, `docs/pages/reference/evm/block-tracking.mdx` | `pollFinalizedBlockNumberScheduled` called in `evm_state_poller.go` |
| `0fcffa82` feat(data): carry the block timestamp through shared state; latest poll back on the shared clock (RHI-8244) | `data/shared_state_block_timestamp.go` (+ `_test`), `architecture/evm/evm_state_poller_shared_clock_test.go`; signature changes in `data/connector.go`, `data/shared_state_variable.go`, call sites in `architecture/evm/evm_state_poller.go`, `architecture/svm/svm_state_poller.go`, `upstream/upstream.go`; `docs/pages/reference/evm/block-tracking.mdx` | `BlockTimestamp` in `data/connector.go`, and `latestBlockShared.TryUpdateIfStale(` (not `IfLocallyStale`) in `evm_state_poller.go` |

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

**RHI-6529 — `rediss://` URI for the Redis rate-limiter store.** Upstream hands
`rateLimiters.store.redis.uri` verbatim to envoyproxy/ratelimit's `NewClientImpl`, which
uses it as the dial address and takes TLS and auth as separate arguments. radix v3
underneath parses `redis://` URLs (credentials, `/N` db) but not `rediss://`, so a TLS URI
of the shape our ElastiCache module emits is dialled as a raw TCP address (`too many
colons in address`), the connect is retried in the background forever, and **every budget
on the store fail-opens** for the life of the process — logged at Warn only, which prod's
`logLevel: error` never prints. There is no config-only workaround: the discrete
`addr`/`username`/`password`/`tls.enabled` form is rebuilt into the same URI by
`SetDefaults`. The patch normalises the connector into a `redis://` URL (credentials, path
and query untouched, so radix keeps doing AUTH and SELECT) and lifts TLS out of the scheme
into envoy's dialer flag plus a `tls.Config` that honours the `tls` block when present.

*Upstreamable:* yes — upstream `main` has the identical code and no issue or PR for it
(checked 2026-09-04); the only TLS ElastiCache path upstream ever exercised is the IAM one
(#952), which side-steps envoy's client. If it lands upstream, delete this patch.

*Rebase risk:* low. The logic is in its own file; only the ~20-line call site in
`connectRedisTask` can conflict. Keep envoy's auth argument **empty** there — credentials
travel in the URL and envoy's argument would override them. The miniredis-over-TLS test
(`TestRateLimitersRegistry_RedissURI_SharedCounter`) fails on the unpatched call site, so
a dropped patch shows up as a test failure, not a silent fail-open.

**RHI-8079 — the corroborated head serves the leader within lag noise.** Upstream 0.3.0
(#1154) made the network head the plain second-highest upstream head, so one wrong-chain
upstream cannot make every honest one read as far behind. With two upstreams — nearly
every network in our prod config — that is always the *lower* head: `latest` is
interpolated to the lagging upstream while `eth_blockNumber` reports the fresher one, so
a client that waits for block N and reads `latest` gets N-1 for up to a poll cycle. On
2026-10-06 that made the orchestrator size four Ethereum deposits from their pre-deposit
balance, and they bridged dust. `CorroboratedHead` serves the highest head when it leads
the second-highest by at most `CorroboratedHeadLeadTolerance` (16, the default policy's
`blockNumberLagAbove`), caps it at the second-highest + 16 for a lead up to
`DefaultToleratedBlockHeadRollback` (1024), and serves the second-highest beyond that (an
outlier). The cap is what keeps the head monotonic: the first version (#12) dropped to the
second-highest past 16, and on dev fast chains (`4663`, `143`, `421614`) the head then
sawtoothed back 8–16 blocks every few seconds as the leader outran a 5s poller. While
capped, the block-time EMA samples the leader's own block and timestamp, never the capped
number. It drives both the tracker head (lag) and the default `latest`/`finalized`
interpolation; majority (`servedTip`) mode is untouched. SVM feeds the same tracker, so its
slot head follows the same rule.

*Upstreamable:* worth proposing, but upstream chose "N=2 → the lower head" deliberately
(see the `PickServedTip` doc comment), so expect a discussion rather than a quick merge.
If it lands upstream, delete this patch.

*Rebase risk:* low for the logic (its own file; a short block in `corroboratedNetworkHead`,
the block-time sample line, and one line in `evmHeadReference`). The likely conflicts are
the upstream test expectations we changed for 1–50 block gaps — keep ours. 0.4.0 (#1208)
rewrote the condition around the block-time sample line; the merge keeps upstream's condition
(`>=`, and resample when the timestamp changes on the same head) and our reporter lookup.
`TestCorroboratedHeadLeadTolerance_MatchesDefaultPolicyLagThreshold` fails if upstream
moves the default policy's lag threshold; the new `*_head_noise_tolerance_test.go` files
fail if either call site is dropped, and `TestTrackerHeadLeaderRunningAwayIsCappedNotDropped`
fails if the head can move back or block time stops being sampled while capped.

**RHI-8113 — the scheduled finalized poll debounces on finality period.** Upstream gates
both the `latest` and the `finalized` state-poller fetch on `resolveDebounce()`, which is
derived from estimated block production time. That is right for the head, which advances
once per block, and wrong for finality: Ethereum finalises at 32-slot/12s epoch
boundaries — 384s — and every chain deriving its `finalized` tag from L1 finality inherits
that cadence while producing blocks every second or two, so the pointer is polled two to
three orders of magnitude more often than it can change. `resolveFinalizedDebounce()`
gates the **scheduled** fetch on `finalityPeriod × 0.7` from a per-chain consensus table;
a chain absent from it falls through to `resolveDebounce()`, which is already correct for
the chains that finalise every block. Measured on prod: 37.4 → 14.6 polls/s, ~$1,017 →
~$411/mo.

The on-demand path (`PollFinalizedBlockNumber`, reached from `EvmIsBlockFinalized` with
`forceFreshIfStale`) deliberately keeps `resolveDebounce()`, so upstream selection is
untouched and only standing background traffic is slowed. Lagging is one-directional: a
finalized pointer behind reality delays cache promotion and can never mark an unfinalized
block as final.

The periods are **code constants, not config**, and must stay that way. eRPC parses config
with `KnownFields(true)`, so a `finalityPeriod` key in the deployed YAML would turn this
patch being dropped in a rebase into an rpc-proxy start-up failure instead of a silent
revert to upstream behaviour — the exact failure this file exists to catch, made worse.

*Upstreamable:* yes, nothing here is Rhinestone-specific — though the table would want a
broader source than our 26 chains before it went up. Never proposed.

*Rebase risk:* low for the logic (its own file), but the **call site is where it dies
quietly**: if a rebase keeps the new file and reverts the two lines in
`evm_state_poller.go`, everything still compiles and the patch becomes a no-op. That is
what the probe checks, and `TestPollFinalizedBlockNumberWiring` fails on it.
`TestFinalityPeriodTable` pins the 17 entries and asserts the 9 deliberate omissions, so a
chain moving between groups cannot pass as a one-character diff. Note Ronin's 240s is the
one value measured rather than derived from a documented mechanism — see RHI-8113.

**RHI-8244 — the block timestamp travels with the head; the `latest` poll stays on the
shared clock.** Upstream 0.4.0 (#1208) moved the EVM `latest` poll from the shared
staleness clock (`TryUpdateIfStale`, one fetch per cluster per debounce window) to a
replica-local one (`TryUpdateIfLocallyStale`, one fetch per pod per window), because a
replica that only ever *received* a head through the shared counter had no block
timestamp to feed its block-time EMA, so its debounce sat on the fixed fallback. Upstream's
own docs now say polling cost scales with replica count; on prod that is a 2–3× multiplier
on the `latest` half of every chain's idle floor. This patch fixes the starvation the other
way round: `CounterInt64State` gains an optional `bt` field, the counter stores it beside
the value on every accepted change and hands both to `OnValue`, and the poller's `latest`
fetch goes back to `TryUpdateIfStale`. A head that arrives without a timestamp
(`SuggestLatestBlock`, or a peer on the previous wire format) is held as unknown; when the
same head arrives again with one, `adoptBlockTimestamp` fills it in once and fires the
callback, which the 0.4.0 tracker already accepts as a matched observation. The field is
`omitempty` and readers ignore unknown fields, so a mixed-version rollout needs no schema
bump.

The signatures changed deliberately rather than adding timestamp-carrying variants beside
the old methods: `getNewValue` returns `(value, blockTimestamp, err)` and `OnValue` takes
`(value, blockTimestamp)`. A clash with upstream at the next sync is the review prompt we
want, not something to route around (decided 2026-10-12).

*Upstreamable:* yes, this is the fix upstream should have shipped instead of #1208, and
the intent is to propose it once it has soaked on prod. If upstream takes it, delete this
patch.

*Rebase risk:* medium. `data/shared_state_variable.go` is upstream-active (#1208 was the
last touch); our edits there are the interface, six short hook lines in `processNewState` /
`processNewValue`, the refresh result, and `localState`. The adoption logic is its own file.
The call site that dies quietly is the one in `PollLatestBlockNumber`: a rebase that takes
upstream's `TryUpdateIfLocallyStale` line compiles and silently restores the per-pod poll,
which is what the probe and `TestPollLatestBlockNumberUsesTheSharedClock` catch.
`TestBlockTimestamp_ReceivingReplicaSamplesBlockTime` fails if the timestamp stops reaching
a receiving replica's tracker.

**Base-10 quantity tolerance.** Some upstreams return EVM quantities as base-10 strings
instead of `0x`-prefixed hex, which broke upstream health tracking. `HexToUint64` /
`HexToInt64` accept both.

*Upstreamable:* probably. Never proposed.

### Config and CI

| Patch | What |
| -- | -- |
| `301093f3` update prod config | `erpc-prod.yaml` — the production network/upstream config |
| `0e618231` Cleanup workflows | deletes upstream's CI (benchmark, codeql, dependency-review, release, publish-npm, scorecards, test, xray). We build an image from upstream code and trust it; upstream CI is noise, and `xray.yml` additionally wants secrets. **New upstream workflows do not conflict — they simply appear.** Check `.github/workflows/` after every sync: 0.3.0 added `xray.yml` and the rebase carried it in silently; 0.4.0 added `publish-npm.yml` and modified `release.yml`, which stops the rebase on a modify/delete conflict — resolve by deleting. |
| `974281fc` Setup minimal CI | our own minimal workflow set |
| `11a80773` Add the upstream-sync script | `sync-with-upstream.sh` (its `promote-to-prod.sh` and `release.sh` were [removed](#removed-patches)) |
| `cc65fa4e` feat(ci): CodeBuild multi-arch images (RHI-5507, #4) | `buildspec-amd64.yml`, `buildspec-arm64.yml`, `.github/workflows/docker.yaml` |
| `6643c1ac` ci: `workflow_dispatch` trigger (RHI-5509, #5) | `.github/workflows/docker.yaml` |

### This file

| Patch | Files | Probe |
| -- | -- | -- |
| `23ac203b` docs: the patch register itself (#6) | `PATCH_LIST.md`, `CLAUDE.md` | `test -f PATCH_LIST.md`, and `grep -q "This is a fork" CLAUDE.md` |

Listed because it is a fork-only file and can be dropped by a sync like any other patch —
and losing it loses the ability to detect that anything else was dropped. `CLAUDE.md` is
upstream's file with our section appended, so a rebase may conflict there: keep the
appended section and take upstream's version of the rest.

## Removed patches

**`174471cd` fix(docker): pin pnpm to the packageManager version** (fork PR #7, merged
2026-09-29, dropped at the 0.4.0 sync on 2026-10-12).

Superseded: upstream #1184 (in 0.4.0) pins `pnpm@10.28.2` in the same place for the same
reason and also drops the duplicate install in `ts-dev`. Our commit applied on top of it as a
no-op diff, so it was dropped rather than carried. The constraint it documented still holds:
the pin and `package.json`'s `packageManager` must stay in step, or `--frozen-lockfile` fails
on a version mismatch. That is now upstream's problem to keep, and a sync that bumps one
without the other shows up as a failed image build, not a silent regression.

**`9d91a08d` (part) treat `ws://`/`wss://` endpoints as providers** (part of "update prod
config", 2026-01-09, dropped at the 0.4.0 sync on 2026-10-12).

Two lines in `convertUpstreamToProvider` exempting websocket endpoints from provider
conversion. No config in any environment uses a `ws://` endpoint, and at 0.3.0 the exemption
could not have produced a working upstream anyway — there was no websocket client behind it.
Upstream `main` above 0.4.0 carries the identical lines as part of the WebSocket work
(#836), so when the fork syncs past that tag the behaviour returns from upstream, with a
client behind it.

**`d8a76c5d` (part) the `release` branch as the production deploy path** — `promote-to-prod.sh`,
`release.sh`, and the `release` entries in `.github/workflows/docker.yaml` (removed 2026-10-08).

Pushing `main` to `release` built an image tagged `prod` and dispatched the infra promote
workflow with `env=prod`. Two things were wrong with it, both found while shipping RHI-8113:

- **It did not describe production.** `release` sat 40 commits behind `main`, while prod was
  running `799f930` — a commit on `main` that had never reached `release`. The deployed
  artifact is recorded in `terraform-aws/environments/argocd/apps/rpc-proxy/values-prod.yaml`
  in the infra repo; the branch was stale bookkeeping that read like a source of truth.
- **It shipped a different image than the one that was soaked.** Promotion rebuilt from
  whatever `main` pointed at, so a docs commit landing after a soak meant prod got an image
  no one had run. Validation on dev did not transfer.

Prod is now promoted by dispatching `aws-promote-app-version.yaml` in `rhinestonewtf/infra`
with `env=prod`, `app_name=rpc-proxy`, and the SHA already on dev, which moves the existing
image rather than building a new one:

```bash
gh workflow run aws-promote-app-version.yaml --repo rhinestonewtf/infra \
  -f env=prod -f app_name=rpc-proxy -f version_sha=<sha already on dev>
```

The run waits on the `prod-apps` GitHub environment for approval. `sync-with-upstream.sh` is
unaffected and still the rebase tool.

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
