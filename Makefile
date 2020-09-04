# https://www.gnu.org/software/make/manual/html_node/Phony-Targets.html
.PHONY: build test test_coverage codecov_coverage format lint bench setup_ci

# Build code with readonly to verify go.mod is up to date in CI
build:
	go build -mod=readonly ./...

# test code with race detector.  Also tests benchmarks (but only for 1ns so they at least run once)
test:
	env "GORACE=halt_on_error=1" go test -v -benchtime 1ns -bench . -race ./...

# Test code with coverage.  Separate from 'test' since covermode=atomic is slow.
test_coverage:
	env "GORACE=halt_on_error=1" go test -v -benchtime 1ns -bench . -covermode=count -coverprofile=coverage.out ./...

# Format your code.  Uses both gofmt and goimports
format:
	gofmt -s -w ./..
	find . -iname '*.go' -print0 | xargs -0 goimports -w

# Lint code for static code checking.  Uses golangci-lint
lint:
	golangci-lint run

# The exact version of CI tools should be specified in your go.mod file and referenced inside your tools.go file
setup_ci:
	go install github.com/golangci/golangci-lint/cmd/golangci-lint

