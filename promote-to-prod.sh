#!/usr/bin/env bash
set -euo pipefail

echo "Promoting main -> release (prod)"
git push origin main:release --force-with-lease
echo "Done! $(git describe --tags --always)"
