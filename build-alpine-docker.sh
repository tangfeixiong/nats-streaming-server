#!/bin/bash
set -o errexit
set -o nounset
set -o pipefail

if [[ $# -eq 0 ]]; then
	echo "Usage: $0 [--push]" >/dev/stderr
	# exit 1
fi

BUILD_ROOT=$(dirname "${BASH_SOURCE[0]}")

DOCKER_BUILD_CONTEXT=$(mktemp -d)

DOCKER_IMAGE="docker.io/tangfeixiong/nats-streaming-server"

cp $BUILD_ROOT/Dockerfile.alpine $DOCKER_BUILD_CONTEXT/Dockerfile
# cp $BUILD_ROOT/seed.conf $DOCKER_BUILD_CONTEXT/

#CGO_ENABLED=0 go install -v -a -tags netgo -installsuffix netgo -ldflags "-s -w -X github.com/nats-io/nats-streaming-server/version.GITCOMMIT=`git rev-parse --short HEAD`"

CGO_ENABLED=0 go build -o ${DOCKER_BUILD_CONTEXT}/nats-streaming-server --installsuffix cgo -ldflags "-X github.com/nats-io/nats-streaming-server/version.GITCOMMIT=`git rev-parse --short HEAD`" -v github.com/nats-io/nats-streaming-server

docker build -t ${DOCKER_IMAGE} ${DOCKER_BUILD_CONTEXT}

[[ 0 < $# ]] && [[ $1 = '--push' ]] && docker push $DOCKER_IMAGE

# Cleanup
rm -rf $DOCKER_BUILD_CONTEXT

docker rmi $(docker images --all --quiet --filter=dangling=true)
