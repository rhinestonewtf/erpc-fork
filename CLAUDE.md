# erpc — Claude Code Guide

See [.cursor/rules/](.cursor/rules/) for all project rules and conventions.

## This is a fork (rhinestonewtf/erpc-fork)

`main` is our commits rebased onto an upstream tag and force-pushed, so **every fork
commit hash is rewritten on each sync**. [PATCH_LIST.md](PATCH_LIST.md) is the register
of what we carry and the source of truth for it.

- **After any upstream sync**, run the probes in PATCH_LIST.md and
  `go test ./architecture/evm/... ./common/...`. A rebase can report success while
  having silently dropped a patch — that has already happened once (see its Removed
  section). Never trust `git log` subjects alone to tell you what our patches are:
  `90800261` is called "update prod config" and also changes two Go files.
- **Never rebuild `main` by resetting to an upstream ref and cherry-picking** what you
  remember. Rebase, or replay from PATCH_LIST.md and verify every probe.
- **Adding a fork patch**: put the logic in a NEW file with a minimal call site in the
  upstream file, so a rebase can only conflict on a few lines; add a row to
  PATCH_LIST.md in the same commit, with a probe and a note on whether it is
  upstreamable; add a test that fails if the patch is dropped or reordered.
- **Before writing a patch**, check whether upstream already fixed it: read
  `git log <our base>..upstream/main -- <the files>` and the open upstream PRs. Two of
  our patches turned out to be superseded upstream.
- Changes go through a PR into `main` and are squash-merged; only `sync-with-upstream.sh`
  force-pushes `main`.
