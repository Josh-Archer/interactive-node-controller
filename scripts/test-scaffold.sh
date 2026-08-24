#!/usr/bin/env bash
set -euo pipefail
test -s README.md
test -s docs/roadmap.md
test -s Makefile
test -s .github/workflows/ci.yml
test -s .github/workflows/release.yml
test -s .github/workflows/workflow-contract.yml
grep -q 'linux/amd64' .github/workflows/release.yml
grep -q 'linux/arm64' .github/workflows/release.yml
grep -q 'permissions:' .github/workflows/ci.yml
grep -q 'permissions:' .github/workflows/release.yml
grep -q 'NodeActivity' README.md docs/roadmap.md
echo 'scaffold contract checks passed'
