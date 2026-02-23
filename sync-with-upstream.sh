#!/usr/bin/env bash
set -euo pipefail

UPSTREAM_REMOTE="upstream"
UPSTREAM_URL="https://github.com/erpc/erpc"
ORIGIN_REMOTE="origin"
BRANCH="main"

usage() {
  echo "Usage: $0 <tag>"
  echo ""
  echo "Syncs the fork with an upstream release tag."
  echo "  1. Ensures upstream remote exists"
  echo "  2. Fetches the tag from upstream"
  echo "  3. Rebases our commits on top of it"
  echo "  4. Pushes main branch and the tag to origin"
  echo ""
  echo "Example: $0 0.0.63"
  exit 1
}

if [[ $# -ne 1 ]]; then
  usage
fi

TAG="$1"

# Ensure we're on the right branch
CURRENT_BRANCH=$(git branch --show-current)
if [[ "$CURRENT_BRANCH" != "$BRANCH" ]]; then
  echo "Error: must be on '$BRANCH' branch (currently on '$CURRENT_BRANCH')"
  exit 1
fi

# Ensure working tree is clean (tracked changes + untracked files)
if ! git diff --quiet || ! git diff --cached --quiet || [[ -n "$(git ls-files --others --exclude-standard)" ]]; then
  echo "Error: working tree is not clean. Commit or stash changes first."
  git status --short
  exit 1
fi

# Ensure upstream remote exists
if ! git remote get-url "$UPSTREAM_REMOTE" &>/dev/null; then
  echo "Adding upstream remote: $UPSTREAM_URL"
  git remote add "$UPSTREAM_REMOTE" "$UPSTREAM_URL"
fi

# Fetch the specific tag from upstream
echo "Fetching tag '$TAG' from upstream..."
git fetch "$UPSTREAM_REMOTE" "refs/tags/$TAG:refs/tags/$TAG"

# Verify the tag exists
if ! git rev-parse "$TAG" &>/dev/null; then
  echo "Error: tag '$TAG' not found after fetch"
  exit 1
fi

# Show what will be rebased
CUSTOM_COMMITS=$(git log --oneline "$TAG"..HEAD 2>/dev/null || true)
if [[ -z "$CUSTOM_COMMITS" ]]; then
  echo "No custom commits to rebase — already up to date with '$TAG'"
  exit 0
fi

echo ""
echo "Custom commits to rebase on top of '$TAG':"
echo "$CUSTOM_COMMITS"
echo ""
read -rp "Proceed with rebase? [y/N] " confirm
if [[ "$confirm" != [yY] ]]; then
  echo "Aborted."
  exit 1
fi

# Rebase our commits on top of the tag
echo "Rebasing on top of '$TAG'..."
git rebase "$TAG"

# Push updated branch and the tag to origin
echo "Pushing '$BRANCH' branch to origin..."
git push "$ORIGIN_REMOTE" "$BRANCH" --force-with-lease

echo "Pushing tag '$TAG' to origin..."
git push "$ORIGIN_REMOTE" "refs/tags/$TAG"

echo ""
echo "Done! Branch '$BRANCH' rebased on '$TAG' and pushed to origin."
echo "Version will be: custom-$(git describe --tags --always)"
