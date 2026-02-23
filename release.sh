#!/usr/bin/env bash
set -euo pipefail

ORIGIN_REMOTE="origin"
SOURCE_BRANCH="main"
RELEASE_BRANCH="release"

# Ensure we're on the right branch
CURRENT_BRANCH=$(git branch --show-current)
if [[ "$CURRENT_BRANCH" != "$SOURCE_BRANCH" ]]; then
  echo "Error: must be on '$SOURCE_BRANCH' branch (currently on '$CURRENT_BRANCH')"
  exit 1
fi

# Ensure working tree is clean
if ! git diff --quiet || ! git diff --cached --quiet; then
  echo "Error: working tree is not clean. Commit or stash changes first."
  git status --short
  exit 1
fi

# Show what will be released
echo "Current $SOURCE_BRANCH: $(git rev-parse --short HEAD)"

RELEASE_EXISTS=$(git rev-parse --verify "$ORIGIN_REMOTE/$RELEASE_BRANCH" 2>/dev/null || echo "")
if [[ -n "$RELEASE_EXISTS" ]]; then
  RELEASE_SHA=$(git rev-parse --short "$ORIGIN_REMOTE/$RELEASE_BRANCH")
  echo "Current $RELEASE_BRANCH: $RELEASE_SHA"

  NEW_COMMITS=$(git log --oneline "$ORIGIN_REMOTE/$RELEASE_BRANCH..HEAD" 2>/dev/null || true)
  if [[ -z "$NEW_COMMITS" ]]; then
    echo "No new commits to release — $RELEASE_BRANCH is already up to date."
    exit 0
  fi

  echo ""
  echo "New commits since last release:"
  echo "$NEW_COMMITS"
else
  echo "Release branch does not exist yet, will create it."
fi

echo ""
read -rp "Push $SOURCE_BRANCH to $RELEASE_BRANCH? [y/N] " confirm
if [[ "$confirm" != [yY] ]]; then
  echo "Aborted."
  exit 1
fi

git push "$ORIGIN_REMOTE" "$SOURCE_BRANCH:$RELEASE_BRANCH" --force-with-lease

echo ""
echo "Done! $RELEASE_BRANCH updated to $(git rev-parse --short HEAD)"
