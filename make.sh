#!/bin/bash

set -ue -o pipefail
if [ "${DEBUG-}" == "true" ]; then
  set -x
fi
# Repo is part of the image name for this build (repo=repository)
REPO=${CIRCLE_PROJECT_USERNAME-cresta}/${CIRCLE_PROJECT_REPONAME-terraform}
# Tag is the image tag of this build's docker file
TAG=${GIT_COMMIT-$(git rev-parse --verify HEAD)}
# The docker image is the repository and tag together
IMAGE=${REPO}:${TAG}

# App is the name of the docker container we execute in dockerrun
APP=check-${CIRCLE_PROJECT_REPONAME-app}
# Volume is how we pass local context to the build container.
VOLUME=mount-${CIRCLE_PROJECT_REPONAME-default}

function build_builder() {
  docker build -t "${IMAGE}-builder" -f builder.dockerfile .
}

function dockerrun() {
  (
    docker rm "${VOLUME}" || true
    docker rm "${APP}" || true
  ) 1> /tmp/stdout 2> /tmp/stderr
  docker create -v /work --name "${VOLUME}" "${IMAGE}" /bin/true >> /tmp/stdout
  docker cp ./ "${VOLUME}:/work"
  # Volume trickery to get around mounted volumes not being usable in circleci docker worker
  docker run -e DEBUG --rm --name "${APP}" --volumes-from "${VOLUME}" "${IMAGE}" "$@"
}

function test() {
  env "GORACE=halt_on_error=1" go test -v -race -benchtime 1ns -bench . ./...
}

function integration_test() {
  env "GORACE=halt_on_error=1" go test --tags=integration -v -benchtime 1ns -bench . -race ./...
}

function build() {
  go build -mod=readonly ./...
}

function reformat() {
  gofmt -s -w ./..
	find . -iname '*.go' -print0 | xargs -0 goimports -w
}

function lint() {
  golangci-lint run
  shellcheck ./make.sh
  hadolint ./Dockerfile
}

declare -a funcs=(reformat check_formatting export_docker import_docker build_docker dockerrun lint build_builder)
for f in "${funcs[@]}"; do
  if [ "${f}" == "${1-}" ]; then
    $f "${@:2}"
    exit $?
  fi
done
echo "Invalid param ${1-}.  Valid options: ${funcs[*]}"
exit 1
