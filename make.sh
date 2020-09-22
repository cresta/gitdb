#!/bin/bash

set -ue -o pipefail
if [ "${DEBUG-}" == "true" ]; then
  set -x
fi

# Repo is part of the image name for this build (repo=repository)
REPO=${CIRCLE_PROJECT_USERNAME-cresta}/${CIRCLE_PROJECT_REPONAME-gitdb}
# Tag is the image tag of this build's docker file
TAG=${TAG-${CIRCLE_SHA1-latest}}
# The docker image is the repository and tag together
IMAGE=${IMAGE-"${REPO}:${TAG}"}
BUILDER_IMAGE=builder-gitdb:${TAG}-builder
VOLUME=mount-${CIRCLE_PROJECT_REPONAME-default}

# App is the name of the docker container we execute in dockerrun
APP=check-${CIRCLE_PROJECT_REPONAME-app}

function build_builder() {
  docker build -t "${BUILDER_IMAGE}" -f builder.dockerfile .
}

function dockerrun() {
  (
    docker rm "${VOLUME}" || true
    docker rm "${APP}" || true
  ) 1> /tmp/stdout 2> /tmp/stderr
  docker create -v /work --name "${VOLUME}" "${BUILDER_IMAGE}" /bin/true >> /tmp/stdout
  docker cp ./ "${VOLUME}:/work"
  # Volume trickery to get around mounted volumes not being usable in circleci docker worker
  docker run -e DEBUG --rm --name "${APP}" --volumes-from "${VOLUME}" "${BUILDER_IMAGE}" "$@"
  (
    docker rm "${VOLUME}" || true
    docker rm "${APP}" || true
  ) 1> /tmp/stdout 2> /tmp/stderr
}

function build_docker() {
    docker build --build-arg "BUILDER_IMAGE=${BUILDER_IMAGE}" -t "${IMAGE}" .
}

function test() {
  env "GORACE=halt_on_error=1" go test -v -race -benchtime 1ns -bench . ./...
}

function integration_test() {
  env "GORACE=halt_on_error=1" go test --tags=integration -v -benchtime 1ns -bench . -race ./...
}

function build() {
  go mod download
  go mod verify
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /go/bin/gitdb -ldflags '-extldflags "-f no-PIC -static"' -tags 'osusergo netgo static_build'
}

function reformat() {
  gofmt -s -w ./..
	find . -iname '*.go' -print0 | xargs -0 goimports -w
}

function lint() {
  golangci-lint run
  shellcheck ./make.sh
  hadolint ./Dockerfile
  hadolint ./builder.dockerfile
}

declare -a funcs=(reformat check_formatting export_docker import_docker build_docker dockerrun lint build_builder build test)
for f in "${funcs[@]}"; do
  if [ "${f}" == "${1-}" ]; then
    $f "${@:2}"
    exit $?
  fi
done
echo "Invalid param ${1-}.  Valid options: ${funcs[*]}"
exit 1
