#!/bin/sh
# Pull the latest PAIM and rebuild. Fails loudly instead of leaving a stale binary.
set -e
cd "$(dirname "$0")/.."

git pull --ff-only

# wails3's subtasks invoke wails3/npm by name; make sure the Go bin dir is visible.
export PATH="$PATH:$HOME/go/bin"

wails3 build

echo ""
echo "Build complete:"
ls -la bin/PAIM
echo "Launch with: ./bin/PAIM"
