# https://www.gnu.org/software/make/manual/html_node/Phony-Targets.html
.PHONY: build test test_coverage codecov_coverage format lint bench setup_ci


# The exact version of CI tools should be specified in your go.mod file and referenced inside your tools.go file
setup_ci:
	go install github.com/golangci/golangci-lint/cmd/golangci-lint

