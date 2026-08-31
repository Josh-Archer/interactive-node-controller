#!/usr/bin/env bash
set -euo pipefail

# Phase 5 Step 1 contract checks: License and contribution terms
test -s LICENSE
grep -q 'Apache License' LICENSE
grep -q 'Version 2.0, January 2004' LICENSE
grep -q 'Interactive Node Controller Contributors' LICENSE
grep -q 'Apache License, Version 2.0' README.md
grep -q 'Apache License, Version 2.0' CONTRIBUTING.md

echo 'phase 5 public release contract checks passed'
