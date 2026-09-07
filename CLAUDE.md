# erpc — Claude Code Guide

See [.cursor/rules/](.cursor/rules/) for all project rules and conventions.

## Design razor: weakest hypothesis, not shortest

Binding for all design and review work in this repo. From Bennett, AGI-23
([arXiv:2301.12987](https://arxiv.org/abs/2301.12987)); full eRPC-tailored
version in [.cursor/rules/erpc.md](.cursor/rules/erpc.md).

Among designs that EXACTLY handle every observed case, prefer the one
committing to the least beyond the data — "explanations should be no more
specific than necessary". Generality lives in a design's extension (the unseen
inputs it still handles correctly), not its form: short/tidy is provably
neither necessary nor sufficient, and a compact regex or clean enum can be
maximally overcommitted.

eRPC's domain is open-ended sets (chains, vendors, methods, error shapes,
client quirks), so:

- The unknown-case fallthrough is the primary path — design and test it first;
  enumerated cases (method tables, error matchers, chain special-cases) are
  optimisations on top and only acceptable when the unmatched path is safe,
  correct, and observable.
- No unforced commitments: no hard-coded method lists, vendor error-string
  matches, chain-ID special cases, or "all vendors we checked do X" thresholds
  unless today's observed data forces them.
- Weaken by DELETING structure (string + discovery over enum, kind + metadata
  over parallel structs, pass-through over interpretation, config over code
  constants) — never by adding speculative abstraction; unexercised machinery
  is itself a commitment.
- Weak ≠ vague: still decide every observed case exactly, and resolve open
  inputs into bounded low-cardinality interfaces (normalized error codes,
  finite behaviours).
- Wire/protocol facts are explicit validated commitments at the edge; measured
  behaviour of N chains/vendors is NOT an invariant — when reality violates a
  bound, delete the bound rather than stacking exceptions.
- Review test: what unseen-but-plausible input does this silently mishandle,
  and what in today's data forces that commitment? If nothing forces it,
  weaken the design.

## This is a fork (rhinestonewtf/erpc-fork)

`main` is our commits rebased onto an upstream tag and force-pushed, so **every fork
commit hash is rewritten on each sync**. [PATCH_LIST.md](PATCH_LIST.md) is the register
of what we carry and the source of truth for it.

- **After any upstream sync**, run the probes in PATCH_LIST.md and
  `go test ./architecture/evm/... ./common/...`. A rebase can report success while
  having silently dropped a patch — that has already happened once (see its Removed
  section). Never trust `git log` subjects alone to tell you what our patches are:
  `343d9615` is called "update prod config" and also changes two Go files.
- **Never rebuild `main` by resetting to an upstream ref and cherry-picking** what you
  remember. Rebase, or replay from PATCH_LIST.md and verify every probe.
- **Adding a fork patch**: put the logic in a NEW file with a minimal call site in the
  upstream file, so a rebase can only conflict on a few lines, and add a test that fails
  if the patch is dropped or reordered. The patch goes in as a PR, squash-merged; then
  commit its PATCH_LIST.md row — probe, upstreamable-or-not, rebase risk — **directly to
  `main`**, since the squash hash only exists once the PR is merged. A patch is not
  finished until that row is in.
- **Before writing a patch**, check whether upstream already fixed it: read
  `git log <our base>..upstream/main -- <the files>` and the open upstream PRs. Two of
  our patches turned out to be superseded upstream.
- Changes go through a PR into `main` and are squash-merged; only `sync-with-upstream.sh`
  force-pushes `main`.
