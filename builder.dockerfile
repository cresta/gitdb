FROM golang:1.15.0-buster AS builder
# Install git.
# Git is required for fetching the dependencies.
# RUN apt-get update && \
# 	apt-get install -y --no-install-recommends \
# 		ca-certificates=20* \
# 		gcc=4:8* \
#        		git=1:2* \
# 		libc-dev  \
# 		make=4.* \
# 	       	tzdata && \
# 	rm -rf /var/lib/apt/lists/*
WORKDIR /work
# Create appuser
ENV USER=appuser
ENV UID=10001
RUN groupadd -r --gid ${UID} ${USER} && useradd --uid ${UID} -m --no-log-init -g ${USER} ${USER}

# Install golangci-lint
ARG GOLANGCI_LINT_VERSION=1.31.0
RUN curl -L -o /tmp/golangci.tar.gz "https://github.com/golangci/golangci-lint/releases/download/v${GOLANGCI_LINT_VERSION}/golangci-lint-${GOLANGCI_LINT_VERSION}-linux-amd64.tar.gz" && \
	tar -C /tmp -zxvf /tmp/golangci.tar.gz && \
	mv /tmp/golangci-lint*/golangci-lint /usr/local/bin

# Install shellcheck
ARG SHELLCHECK_VERSION=v0.7.1
RUN apt-get update && apt-get install -y xz-utils
RUN curl -L -o /tmp/shellcheck.tar.xz "https://github.com/koalaman/shellcheck/releases/download/${SHELLCHECK_VERSION}/shellcheck-${SHELLCHECK_VERSION}.linux.x86_64.tar.xz" && \
    tar -C /tmp -xJv < /tmp/shellcheck.tar.xz && \
	mv /tmp/shellcheck-${SHELLCHECK_VERSION}/shellcheck /usr/local/bin

# Install hadolint
ARG HADOLINT_VERSION=v1.17.5
RUN curl -L -o /usr/local/bin/hadolint "https://github.com/hadolint/hadolint/releases/download/${HADOLINT_VERSION}/hadolint-Linux-x86_64" && chmod a+x /usr/local/bin/hadolint
ENTRYPOINT ["/work/make.sh"]