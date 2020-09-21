FROM golang:1.14.0-buster AS builder
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
WORKDIR /build
# Create appuser
ENV USER=appuser
ENV UID=10001
RUN groupadd -r --gid ${UID} ${USER} && useradd --uid ${UID} -m --no-log-init -g ${USER} ${USER}

# Install golangci-lint
ARG GOLANGCI_LINT_VERSION=1.24.0
RUN curl -L -o /tmp/golangci.tar.gz "https://github.com/golangci/golangci-lint/releases/download/v${GOLANGCI_LINT_VERSION}/golangci-lint-${GOLANGCI_LINT_VERSION}-linux-amd64.tar.gz" && \
	tar -C /tmp -zxvf /tmp/golangci.tar.gz && \
	mv /tmp/golangci-lint*/golangci-lint /usr/local/bin

COPY . .
RUN go mod download
RUN go mod verify


RUN make build test lint
RUN GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /go/bin/gitdb -ldflags '-extldflags "-f no-PIC -static"' -tags 'osusergo netgo static_build'

FROM scratch
# Import from builder.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /etc/passwd /etc/passwd
COPY --from=builder /etc/group /etc/group
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

# Copy our static executable
COPY --from=builder /go/bin/gitdb /go/bin/gitdb
# Use an unprivileged user.
USER appuser:appuser

EXPOSE 8080
# Run the hello binary.
ENTRYPOINT ["/go/bin/gitdb"]
