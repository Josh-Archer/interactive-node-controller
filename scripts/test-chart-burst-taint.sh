#!/usr/bin/env bash
set -euo pipefail

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

render() {
  helm template interactive-node-controller chart "$@"
}

controller_args() {
  python3 - "$1" <<'PY'
import sys
import yaml

with open(sys.argv[1]) as handle:
    documents = list(yaml.safe_load_all(handle))
deployments = [doc for doc in documents if doc and doc.get("kind") == "Deployment"]
if len(deployments) != 1:
    raise SystemExit(f"expected 1 Deployment, found {len(deployments)}")
args = deployments[0]["spec"]["template"]["spec"]["containers"][0]["args"]
print("\n".join(args))
PY
}

assert_has() {
  local file=$1 needle=$2
  if ! grep -qx -- "$needle" "$file"; then
    echo "missing arg: $needle" >&2
    echo "args:" >&2
    cat "$file" >&2
    exit 1
  fi
}

assert_lacks() {
  local file=$1 needle=$2
  if grep -q -- "$needle" "$file"; then
    echo "unexpected arg matching: $needle" >&2
    cat "$file" >&2
    exit 1
  fi
}

expect_fail() {
  local needle=$1
  shift
  if render "$@" >"$tmp/fail-out.yaml" 2>"$tmp/fail-err.txt"; then
    echo "expected chart render to fail for: $needle" >&2
    exit 1
  fi
  if ! grep -q -- "$needle" "$tmp/fail-err.txt"; then
    echo "render failed without expected text: $needle" >&2
    cat "$tmp/fail-err.txt" >&2
    exit 1
  fi
}

render >"$tmp/unset.yaml"
controller_args "$tmp/unset.yaml" >"$tmp/unset-args.txt"
assert_lacks "$tmp/unset-args.txt" '--burst-taint-key'
assert_has "$tmp/unset-args.txt" '--taint-key=availability.interactive-node.io/state'
assert_has "$tmp/unset-args.txt" '--fail-closed=true'
assert_has "$tmp/unset-args.txt" '--eviction-enabled=false'

render --set-string taint.burstKey= >"$tmp/empty.yaml"
controller_args "$tmp/empty.yaml" >"$tmp/empty-args.txt"
assert_lacks "$tmp/empty-args.txt" '--burst-taint-key'
cmp -s "$tmp/unset-args.txt" "$tmp/empty-args.txt"

render -f examples/helm-values.yaml >"$tmp/example-values.yaml"
controller_args "$tmp/example-values.yaml" >"$tmp/example-args.txt"
assert_lacks "$tmp/example-args.txt" '--burst-taint-key'

render --set-string taint.burstKey=availability.interactive-node.io/burst >"$tmp/configured.yaml"
controller_args "$tmp/configured.yaml" >"$tmp/configured-args.txt"
assert_has "$tmp/configured-args.txt" '--burst-taint-key=availability.interactive-node.io/burst'
assert_has "$tmp/configured-args.txt" '--taint-key=availability.interactive-node.io/state'
test "$(grep -c -- '--burst-taint-key=' "$tmp/configured-args.txt")" -eq 1

render --set-string taint.burstKey=burst >"$tmp/plain.yaml"
controller_args "$tmp/plain.yaml" >"$tmp/plain-args.txt"
assert_has "$tmp/plain-args.txt" '--burst-taint-key=burst'

render --set-string taint.key=custom.example/state --set-string taint.burstKey=custom.example/burst >"$tmp/both.yaml"
controller_args "$tmp/both.yaml" >"$tmp/both-args.txt"
assert_has "$tmp/both-args.txt" '--taint-key=custom.example/state'
assert_has "$tmp/both-args.txt" '--burst-taint-key=custom.example/burst'

name63=$(printf 'a%.0s' {1..63})
render --set-string "taint.burstKey=example.com/${name63}" >"$tmp/name63.yaml"
controller_args "$tmp/name63.yaml" >"$tmp/name63-args.txt"
assert_has "$tmp/name63-args.txt" "--burst-taint-key=example.com/${name63}"

prefix253=$(printf 'a%.0s' {1..253})
render --set-string "taint.burstKey=${prefix253}/burst" >"$tmp/prefix253.yaml"
controller_args "$tmp/prefix253.yaml" >"$tmp/prefix253-args.txt"
assert_has "$tmp/prefix253-args.txt" "--burst-taint-key=${prefix253}/burst"

expect_fail 'collides with taint.key' --set-string taint.burstKey=availability.interactive-node.io/state
expect_fail 'collides with taint.key' --set-string taint.key=example.com/shared --set-string taint.burstKey=example.com/shared
expect_fail 'cannot contain whitespace' --set-string 'taint.burstKey=bad key'
expect_fail 'is not a valid Kubernetes taint key' --set-string taint.burstKey=Example.com/burst
expect_fail 'is not a valid Kubernetes taint key' --set-string taint.burstKey=/burst
expect_fail 'is not a valid Kubernetes taint key' --set-string taint.burstKey=example.com/burst-
expect_fail 'is not a valid Kubernetes taint key' --set-string taint.burstKey=example.com/a/b
name64=$(printf 'a%.0s' {1..64})
expect_fail 'is not a valid Kubernetes taint key' --set-string "taint.burstKey=example.com/${name64}"
prefix254=$(printf 'a%.0s' {1..254})
expect_fail 'is not a valid Kubernetes taint key' --set-string "taint.burstKey=${prefix254}/burst"

cat >"$tmp/number-values.yaml" <<'EOF'
taint:
  burstKey: 1
EOF
expect_fail 'taint.burstKey must be a string' -f "$tmp/number-values.yaml"

test -s examples/disposable-runner.yaml
grep -q 'availability.interactive-node.io/schedulable' examples/disposable-runner.yaml
grep -q 'interactive-node-controller.io/evictable: "true"' examples/disposable-runner.yaml
grep -q 'interactive-node-controller.io/allow-lossy-eviction: "true"' examples/disposable-runner.yaml
grep -q 'NoSchedule' examples/disposable-runner.yaml
grep -q 'emptyDir' examples/disposable-runner.yaml
grep -q 'Loss of scratch data and interrupted work are part of the lossy contract.' examples/disposable-runner.yaml
grep -q 'consuming GitOps repository' examples/disposable-runner.yaml
grep -q 'burstKey:' chart/values.yaml
grep -q 'burstTaintArg' chart/templates/deployment.yaml

echo 'chart burst taint render checks passed'
