.PHONY: build-linux-amd64 fmt fmt-check test vet contracts ansible-syntax verify

GO_FILES := $(shell find . -name '*.go' -type f -not -path './vendor/*')

build-linux-amd64:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o bin/node-activity-reporter-linux-amd64 ./cmd/node-activity-reporter

fmt:
	gofmt -w $(GO_FILES)

fmt-check:
	@test -z "$$(gofmt -l $(GO_FILES))" || { gofmt -l $(GO_FILES); exit 1; }

test:
	go test ./...

vet:
	go vet ./...

contracts:
	./scripts/test-scaffold.sh
	./scripts/test-phase1-contract.sh

ansible-syntax:
	ANSIBLE_ROLES_PATH=ansible/roles ansible-playbook --syntax-check ansible/syntax-check.yml

verify: fmt-check test vet contracts
