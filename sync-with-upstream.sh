#!/usr/bin/env bash
set -euo pipefail

UPSTREAM_REMOTE="upstream"
UPSTREAM_URL="https://github.com/erpc/erpc"
ORIGIN_REMOTE="origin"
BRANCH="main"

usage() {
  echo "Usage: $0 <tag-or-branch>"
  echo ""
  echo "Syncs the fork with an upstream release tag or branch."
  echo "  1. Ensures upstream remote exists"
  echo "  2. Fetches the ref from upstream"
  echo "  3. Rebases our commits on top of it"
  echo "  4. Pushes main branch (and the tag, if applicable) to origin"
  echo ""
  echo "Examples:"
  echo "  $0 0.0.63       # sync to a release tag"
  echo "  $0 main          # sync to upstream main branch"
  exit 1
}

if [[ $# -ne 1 ]]; then
  usage
fi

REF="$1"

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

# Determine if REF is a tag or a branch
IS_TAG=false
if git ls-remote --tags "$UPSTREAM_REMOTE" "$REF" | grep -q "$REF"; then
  IS_TAG=true
fi

# Fetch from upstream
if $IS_TAG; then
  echo "Fetching tag '$REF' from upstream..."
  git fetch "$UPSTREAM_REMOTE" "refs/tags/$REF:refs/tags/$REF"
  REBASE_TARGET="$REF"
else
  echo "Fetching branch '$REF' from upstream..."
  git fetch "$UPSTREAM_REMOTE" "$REF"
  REBASE_TARGET="$UPSTREAM_REMOTE/$REF"
fi

# Verify the target exists
if ! git rev-parse "$REBASE_TARGET" &>/dev/null; then
  echo "Error: '$REBASE_TARGET' not found after fetch"
  exit 1
fi

# Show what will be rebased
CUSTOM_COMMITS=$(git log --oneline "$REBASE_TARGET"..HEAD 2>/dev/null || true)
if [[ -z "$CUSTOM_COMMITS" ]]; then
  echo "No custom commits to rebase — already up to date with '$REF'"
  exit 0
fi

echo ""
echo "Custom commits to rebase on top of '$REF' ($(git rev-parse --short "$REBASE_TARGET")):"
echo "$CUSTOM_COMMITS"
echo ""
read -rp "Proceed with rebase? [y/N] " confirm
if [[ "$confirm" != [yY] ]]; then
  echo "Aborted."
  exit 1
fi

# Rebase our commits on top of the target
echo "Rebasing on top of '$REBASE_TARGET'..."
git rebase "$REBASE_TARGET"

# Push updated branch to origin
echo "Pushing '$BRANCH' branch to origin..."
git push "$ORIGIN_REMOTE" "$BRANCH" --force-with-lease

# Push the tag to origin if applicable
if $IS_TAG; then
  echo "Pushing tag '$REF' to origin..."
  git push "$ORIGIN_REMOTE" "refs/tags/$REF"
fi

CUSTOM_COUNT=$(git log --oneline "$REBASE_TARGET"..HEAD | wc -l | tr -d ' ')
SHORT_SHA=$(git rev-parse --short HEAD)
VERSION="${REF}-${CUSTOM_COUNT}-g${SHORT_SHA}"

echo ""
echo "Done! Branch '$BRANCH' rebased on '$REF' and pushed to origin."
echo "Version: ${VERSION}"
