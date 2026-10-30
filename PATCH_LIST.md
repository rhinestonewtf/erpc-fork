# Fork patch list

Every change this fork carries on top of upstream `erpc/erpc`. **This file is the
source of truth for what "our patches" are.**

Workflow: the patch itself goes in through a PR and is **squash-merged**; its row here is
then committed **directly to `main`**, because the squash hash does not exist until
after the merge. Fork PRs #20 and #21 were merged with merge commits instead (13
commits), and the 0.5.0 sync collapsed them to one commit per PR; squash at merge time
so a sync does not have to. If you drop a patch, move it to [Removed](#removed-patches) with the
reason.

Currently rebased onto upstream **`0.5.0`** (`204f98cb`, released 2026-10-27).

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

That last point is why `b85410ad` deserves a warning: it is named *"update prod
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
grep -q "BlockTimestamp" data/connector.go && echo OK
grep -q "latestBlockShared.TryUpdateIfStale(" architecture/evm/evm_state_poller.go && echo OK
grep -q "cacheBlockByHash(" architecture/evm/evm_state_poller.go && echo OK
grep -B1 "cacheBlockByHash(" architecture/evm/evm_state_poller.go | grep -q 'blockTag == "latest"' && echo OK
grep -q "hub.hashFor(" erpc/block_stream.go && echo OK
grep -q "BlockHash" data/connector.go && echo OK
! grep -rqE 'processNewValue\([A-Za-z]+, [a-z]+\.Value\)|OnLatestBlock\(func\(int64\)\)' --include='*.go' . && echo OK
test -f erpc-prod.yaml && echo OK
test -f buildspec-amd64.yml && echo OK
test -f buildspec-arm64.yml && echo OK
test -f .github/workflows/docker.yaml && echo OK
test -f sync-with-upstream.sh && echo OK
test -f PATCH_LIST.md && echo OK
grep -q "This is a fork" CLAUDE.md && echo OK
grep -q "execdiag.Configure(" erpc/init.go && echo OK
grep -q "execdiag.BeginRequest(" erpc/projects.go && echo OK
grep -q "execdiag.BeginUpstreamCall(" erpc/networks.go && echo OK
grep -q "execdiag.BeginInvocation(" upstream/upstream.go && echo OK
grep -q "recordBreakerRejection(" upstream/upstream_executor.go && echo OK
grep -q "MarkDispatchAttempted(" clients/http_json_rpc_client.go && echo OK
```

A rebase that reports success while a probe fails means the patch was dropped during
conflict resolution. Recover it from the pre-rebase `main` in the reflog
(`git reflog main`), not from a rewritten branch.

Go patches also need `go test ./architecture/evm/... ./common/... ./health/... ./data/... ./upstream/... ./clients/... ./execdiag/... ./erpc/...`
to pass — a patch can survive a rebase textually and still be defeated by upstream logic
that moved around it. The `erpc` package takes ~20 minutes; run it anyway (the quick
subset for the diagnostics patch is `-run 'ExecutionDiagnostics|ExecDiag|DispatchProbe'`
over `./erpc/ ./upstream/ ./clients/`).

RHI-8244 and RHI-8178 changed the shared counter's signatures (`CounterInt64Observation`
in place of a bare `int64`), so **every counter call site upstream adds compiles only after
adaptation**, and a rebase can auto-merge one that compiles wrong: 0.5.0 did it twice
(`scheduleBackgroundPushCurrent` adopting a remote value, and the SVM
`firstAvailableSlotShared` counter). The negative grep in the probe block is the check for
the two known shapes; `go build ./...` is the other half.

## Active patches

### Code

| Patch | Files | Probe |
| -- | -- | -- |
| `0d156024` fix(evm): treat gas-limit rejections as terminal, not retryable (RHI-6277, #6) | `architecture/evm/gas_limit_errors.go`, `architecture/evm/gas_limit_errors_test.go`, `architecture/evm/error_normalizer.go` | `isTerminalGasLimitRejection` present in `error_normalizer.go` |
| `b85410ad` (part of "update prod config") base-10 EVM quantity tolerance | `common/utils.go` | `strconv.ParseUint(s, 10, 64)` in `common/utils.go` |
| `de6d1ee5` fix(ratelimiter): parse rediss:// URI for the Redis store (RHI-6529, #9) | `upstream/ratelimiter_redis_target.go`, `upstream/ratelimiter_redis_target_test.go`, `upstream/ratelimiter_registry.go` | `resolveRateLimiterRedisTarget` called in `ratelimiter_registry.go` |
| `9d59dc1d` feat(data): carry the block timestamp through shared state; latest poll back on the shared clock (RHI-8244) | `data/shared_state_observation.go` (+ `_test`; was `shared_state_block_timestamp.go` until RHI-8178 generalised it), `architecture/evm/evm_state_poller_shared_clock_test.go`; signature changes in `data/connector.go`, `data/shared_state_variable.go`, call sites in `architecture/evm/evm_state_poller.go`, `architecture/svm/svm_state_poller.go`, `upstream/upstream.go`; `docs/pages/reference/evm/block-tracking.mdx` | `BlockTimestamp` in `data/connector.go`, and `latestBlockShared.TryUpdateIfStale(` (not `IfLocallyStale`) in `evm_state_poller.go` |
| `a65431c8` feat(evm): cache the poller's block by hash and stream it by hash (RHI-8178, #16; RHI-8260 #17 folded in at the 0.5.0 sync) | `architecture/evm/evm_state_poller_block_cache.go` (+ `_test`), `data/shared_state_observation.go` (+ `_test`), hub + subscriber loop in `erpc/block_stream.go` (+ `_test`); call sites in `architecture/evm/evm_state_poller.go` (`fetchBlock`, `OnLatestBlock`), `erpc/projects_registry.go`, `data/connector.go`, `data/shared_state_variable.go`; signature adapters in `architecture/svm/svm_state_poller.go`, `upstream/upstream.go`; `docs/pages/reference/evm/block-tracking.mdx` | `cacheBlockByHash(` called in `evm_state_poller.go` and guarded by `if blockTag == "latest"` (RHI-8260, `TestPollFinalizedBlock_IsNotCachedByHash`), `hub.hashFor(` used in `erpc/block_stream.go`, `BlockHash` in `data/connector.go` |
| `1536698e` feat(observability): distinguish upstream dispatch from local rejection (RHI-8707, #20; squash of `672465a8`…`3fa279eb`) | New: `execdiag/`; `common/exec_invocations.go`, `common/dispatch_probe.go`, `common/config_execution_diagnostics.go`; `erpc/networks_execution_diagnostics.go`, `upstream/upstream_execution_diagnostics.go`; tests: `*_execution_diagnostics_test.go`, `clients/dispatch_probe_test.go`, `common/exec_invocations_test.go`, `common/config_execution_diagnostics_test.go`. Call sites: `erpc/init.go`, `erpc/projects.go`, `erpc/http_server.go`, `erpc/multiplexer.go`; `erpc/networks.go` (`Network.Forward` → `forward`, `doForward`, `handleBlockSkip`, `handleMultiplexing`); `erpc/network_executor.go` (hedge leg tag); `upstream/upstream.go`, `upstream/upstream_executor.go`; `clients/http_json_rpc_client.go`, `clients/grpc_bds_client.go`, `clients/grpc_bds_resilience.go`; `common/exec_state.go`, `common/config.go`, `common/defaults.go`, `common/validation.go`. Other: generated `typescript/config` types and `docs/pages/operation/tracing.mdx`. | `execdiag.Configure(` in `erpc/init.go`; `execdiag.BeginRequest(` in `erpc/projects.go`; `execdiag.BeginUpstreamCall(` in `erpc/networks.go`; `execdiag.BeginInvocation(` in `upstream/upstream.go`; `recordBreakerRejection(` in `upstream/upstream_executor.go`; `MarkDispatchAttempted(` in `clients/http_json_rpc_client.go` |
| `01bf9b51` fix(observability): align execution diagnostics edge cases and docs (RHI-8784, #21; squash of `eb5be9f9`…`64d863e1`) | `execdiag/classify.go`, `execdiag/config.go` (+ tests), `erpc/init.go`, `erpc/networks_execution_diagnostics_test.go`, `common/config_execution_diagnostics_test.go`, `docs/pages/operation/tracing.mdx` | covered by the RHI-8707 probes; `TestClassify*` in `execdiag/` pins the leafless-exhaustion classification |

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

**RHI-8178 — the poller's block is cached by hash and streamed by hash.** The state
poller fetches a full block (`eth_getBlockByNumber`, hashes-only) every tick and, before
this patch, kept only the number: its request goes straight to the upstream, so nothing it
observes ever reached the cache, and the `StreamBlocks` subscriber loop fetched the same
block again by number. On a 1-block/s chain that duplicate was ~20 CU/s per chain, 70–85%
of chain-indexer's idle overhead. The cache is the transport: `fetchBlock` writes the `latest` block
into the network's JSON-RPC cache as the response to `eth_getBlockByHash(hash, false)`,
synchronously, before the head is published (the `finalized`-tag fetch is not cached: nothing
reads it by hash, and at write time it classifies as unfinalized because the head it names is
not stored yet — RHI-8260, measured as a quarter of the by-hash writes on dev); the shared head record carries the hash next
to the RHI-8244 timestamp (`CounterInt64Observation` replaces the `(value, timestamp)`
pair, and the counter fills in or supersedes the block fields of a head it already holds);
the hub records number → hash and the subscriber loop fetches a known block by hash through
the normal request path — a shared-cache hit on any replica — and by number only for gaps.
Any by-hash miss, null or error, degrades to the by-number fetch, so the patch is
behaviour-neutral until the environment has an `unfinalized` cache policy for
`eth_getBlockByHash` (infra config; prod caches by hash at `finalized` only). A by-hash
entry is content-addressed, so that policy's TTL is a memory bound, not a correctness knob.

*Upstreamable:* yes, together with RHI-8244, once soaked.

*Rebase risk:* medium, for the same reason as RHI-8244 (the counter file is upstream-active).
The call sites that die quietly are the one line in `fetchBlock` that calls
`cacheBlockByHash` and the `hub.hashFor` lookup in the subscriber loop: either can be
dropped and everything compiles and streams as before, which is what the probes and
`TestPollLatestBlock_CachesTheBlockByHashBeforePublishingTheHead` /
`TestBlockStreamHub_RecordsObservedHashes` catch. The registry wiring (`SetBlockCache` next
to `SetNetworkConfig`) is the third; without it the poller has no cache and writes nothing.

**RHI-8707 — execution diagnostics: dispatched vs locally rejected, per invocation.** A
payload-free, budgeted log stream, independent of `logLevel`, records for each upstream
invocation whether dispatch happened. It exists because breaker rejections and dispatches
were indistinguishable in production logs and metrics. The RHI-8784 follow-up classifies
leafless upstream exhaustion as `rejected` and aligns the documentation.

*Upstreamable:* plausibly. Nothing is Rhinestone-specific, but it is large and opinionated.
Never proposed.

*Rebase risk:* medium. The logic lives in fork-only files, but its call sites sit in
upstream-active `erpc/networks.go`, `upstream/upstream.go`, `upstream/upstream_executor.go`,
and `clients/*`.

*Silent-drop analysis:* Dropping `execdiag.Configure(` means the stream never activates;
tests use `ConfigureForTest`, so only its probe catches that. Dropping `BeginRequest` in
`projects.go` is caught by `TestExecutionDiagnostics_OutcomeIsAfterNetworkPostForward`,
`BeginUpstreamCall` by `TestExecutionDiagnostics_UpstreamPreHookRejection`, and
`recordBreakerRejection` by `TestExecutionDiagnostics_OpenBreakerZeroClientCalls` /
`TestExecDiag_BreakerAdmissionAndRejection`. Dropping `MarkDispatchAttempted` is caught by
`TestDispatchProbe_HttpSingle`, and dropping the inbound-trace hook by
`TestHttpServer_ExecutionDiagnostics_InboundTraceIsLogOnly`. The `Forward`/`forward` rename
fails loudly at compile time in either direction (duplicate or missing method).

*0.5.0 sync note (2026-10-30):* applied as two three-way patches. Conflicts were the
import blocks in `erpc/networks.go` and `erpc/http_server.go` (keep both), the
`NewUpstream` line next to upstream's new Multicall3 wiring (keep both), and `doForward`,
where upstream's `stampClientResponseID` wrapper and the diagnostics scope both sit: the
diagnostics scope is outermost and the stamp happens inside it, in `forwardWithHooks`
(named returns). The `typescript/config/lib` outputs are rebuilt with `pnpm build`, not
merged.

**Base-10 quantity tolerance.** Some upstreams return EVM quantities as base-10 strings
instead of `0x`-prefixed hex, which broke upstream health tracking. `HexToUint64` /
`HexToInt64` accept both.

*Upstreamable:* probably. Never proposed.

### Config and CI

| Patch | What |
| -- | -- |
| `b85410ad` update prod config | `erpc-prod.yaml` — the production network/upstream config |
| `322a523c` Cleanup workflows | deletes upstream's CI (benchmark, codeql, dependency-review, release, publish-npm, scorecards, test, xray). We build an image from upstream code and trust it; upstream CI is noise, and `xray.yml` additionally wants secrets. **New upstream workflows do not conflict — they simply appear.** Check `.github/workflows/` after every sync: 0.3.0 added `xray.yml` and the rebase carried it in silently; 0.4.0 added `publish-npm.yml` and modified `release.yml`, which stops the rebase on a modify/delete conflict — resolve by deleting; 0.5.0 modified `release.yml` and `test.yml` (same conflict, delete both) and added `codecov.yml` at the repo root, which is carried as a harmless upstream file. |
| `66e5867d` Setup minimal CI | our own minimal workflow set |
| `2b46f4b9` Add the upstream-sync script | `sync-with-upstream.sh` (its `promote-to-prod.sh` and `release.sh` were [removed](#removed-patches)) |
| `07d55f3f` feat(ci): CodeBuild multi-arch images (RHI-5507, #4) | `buildspec-amd64.yml`, `buildspec-arm64.yml`, `.github/workflows/docker.yaml` |
| `6ac5a118` ci: `workflow_dispatch` trigger (RHI-5509, #5) | `.github/workflows/docker.yaml` |

### This file

| Patch | Files | Probe |
| -- | -- | -- |
| `0d156024` docs: the patch register itself (#6) | `PATCH_LIST.md`, `CLAUDE.md` | `test -f PATCH_LIST.md`, and `grep -q "This is a fork" CLAUDE.md` |

Listed because it is a fork-only file and can be dropped by a sync like any other patch —
and losing it loses the ability to detect that anything else was dropped. `CLAUDE.md` is
upstream's file with our section appended, so a rebase may conflict there: keep the
appended section and take upstream's version of the rest.

## Removed patches

**`a503823d` fix(health): serve the corroborated head within lag noise, capped** (RHI-8079,
fork PRs #12 and #13, merged 2026-10-07/08, dropped at the 0.5.0 sync on 2026-10-30).

Served the leading upstream's head as the network head when it led the second-highest by at
most 16 blocks, capped at second-highest + 16 for a lead up to 1024, so that two-upstream
networks stopped interpolating `latest` to the laggard while `eth_blockNumber` reported the
leader (the 2026-10-06 deposit-sizing incident).

Dropped because upstream 0.5.0 (#1263, #1256, #1296, #1274) rewrote the whole served-head
stack and removed both call sites. There is no default-vs-majority mode any more: every
config goes through one `servedTip` pipeline with `servedTip.quorum` (default 2 = the
second-highest head; `1` = the highest), stalled heads are benched out of the ballot and the
lag reference after `max(30s, 3 × poll period)`, `eth_blockNumber` is answered from that
same served tip without dispatching, and lag credits a head's age since its last poll. The
incident's inconsistency — `eth_blockNumber` fresh, `latest` interpolated low — is gone by
construction; what remains is freshness: with two upstreams `latest` is the lower head for up
to one poll cycle. A port exists (`dryrun-050`, 2026-10-29: `health/head_noise_tolerance.go`,
`erpc/networks_head_noise_tolerance.go`, applied after the regression guard) but it fails six
subtests of upstream's `TestServedTip_StalledHeadIsBenched` and would need re-deriving at
every sync. The config-only alternative is `servedTip.quorum: 1` on the networks that need
the fresher head, at the cost of trusting a lone far-ahead head until benching kicks in. Not
set as of this sync; decide from dev numbers.

**`61db4a8e` feat(evm): debounce the finalized poll on finality period, not block time**
(RHI-8113, fork PR #14, merged 2026-10-08, dropped at the 0.5.0 sync on 2026-10-30).

Superseded by upstream #1265 and #1269 (in 0.5.0): `evm.finalizedDebounceDepthMultiplier`
(default 0.05) widens every finalized poll to `(latest − finalized) × EMA block time ×
multiplier`, with a catch-up rule ours lacked (an upstream behind the network's finalized
block keeps the regular cadence). It is the weaker hypothesis — no per-chain consensus
table — and its method is literally named `resolveFinalizedDebounce`, so ours could not
coexist without a rename. Upstream's default polls finalized roughly 4–5× more often than
our table did on the listed chains (≈40–60s vs 269s on Ethereum-finality chains); if that
cost matters, raise the multiplier per network in config rather than carrying code. The
17-entry period table is in fork PR #14 if a source is ever wanted upstream.

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
client behind it. That happened at the 0.5.0 sync (2026-10-30): `ws://`/`wss://` upstreams are
now real WebSocket clients upstream, so this entry is closed.

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
