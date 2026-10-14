.PHONY: build-linux-amd64 build-controller fmt fmt-check test test-api test-kind vet contracts ansible-syntax chart-lint docker-build verify

GO_FILES := $(shell find . -name '*.go' -type f -not -path './vendor/*')

build-linux-amd64:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o bin/node-activity-reporter-linux-amd64 ./cmd/node-activity-reporter

build-controller:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o bin/interactive-node-controller-linux-amd64 ./cmd/interactive-node-controller

fmt:
	gofmt -w $(GO_FILES)

fmt-check:
	@test -z "$$(gofmt -l $(GO_FILES))" || { gofmt -l $(GO_FILES); exit 1; }

test:
	go test ./...

# Starts only the local envtest API server; it never reads the active kubeconfig.
test-api:
	XDG_CACHE_HOME=/tmp/interactive-node-controller-envtest KUBEBUILDER_ASSETS="$$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.0.0-20250517180713-32e5e9e948a5 use -p path 1.32.0)" go test ./internal/controller -run 'Test(EnrollmentAPIIntegration|EvictionAPIPreconditions)$$' -count=1 -v

# Requires KUBECONFIG to point at a disposable Kind cluster (kind-* context).
test-kind:
	KIND_INTEGRATION=true go test ./internal/controller -run '^TestEvictionKindPDBSafety$$' -count=1 -v

vet:
	go vet ./...

contracts:
	./scripts/test-scaffold.sh
	./scripts/test-phase1-contract.sh
	./scripts/test-phase2-contract.sh
	./scripts/test-phase3-contract.sh
	./scripts/test-phase4-contract.sh
	./scripts/test-phase5-contract.sh

ansible-syntax:
	ANSIBLE_ROLES_PATH=ansible/roles ansible-playbook --syntax-check ansible/syntax-check.yml

chart-lint:
	helm lint chart
	helm template interactive-node-controller chart >/dev/null

docker-build:
	docker build -t interactive-node-controller:local .

verify: fmt-check test vet contracts chart-lint
