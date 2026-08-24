.PHONY: test lint verify
test:
	./scripts/test-scaffold.sh
lint:
	command -v go >/dev/null && gofmt -w $$(find . -name '*.go' -type f) || true
verify: test
	@if command -v go >/dev/null && test -f go.mod; then go test ./...; fi
