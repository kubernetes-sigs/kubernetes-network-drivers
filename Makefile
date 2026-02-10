# Copyright The Kubernetes Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

REPO_ROOT:=${CURDIR}
OUT_DIR=$(REPO_ROOT)/bin

# disable CGO by default for static binaries
CGO_ENABLED=0
export GOROOT GO111MODULE CGO_ENABLED

build: build-hostdevice

build-hostdevice:
	go build -v -o "$(OUT_DIR)/hostdevice" ./drivers/hostdevice

clean:
	rm -rf "$(OUT_DIR)/"

test:
	CGO_ENABLED=1 go test -v -race -count 1 ./...

e2e-test:
	bats --verbose-run tests/

# code linters
lint:
	hack/lint.sh

update:
	go mod tidy

.PHONY: ensure-buildx
ensure-buildx:
	./hack/init-buildx.sh

# docker image registry, default to upstream
REGISTRY?=gcr.io/k8s-staging-networking
# tag based on date-sha
TAG?=$(shell echo "$$(date +v%Y%m%d)-$$(git describe --always --dirty)")
PLATFORMS?=linux/amd64,linux/arm64

# required to enable buildx
export DOCKER_CLI_EXPERIMENTAL=enabled
image: image-build
image-build: image-build-hostdevice

image-build-hostdevice: ensure-buildx
	docker buildx build -f drivers/hostdevice/Dockerfile . \
		--tag="${REGISTRY}/hostdevice:${TAG}" \
		--platform="${PLATFORMS}" \
		--load

image-push: image-push-hostdevice

image-push-hostdevice: ensure-buildx
	docker buildx build -f drivers/hostdevice/Dockerfile . \
		--tag="${REGISTRY}/hostdevice:${TAG}" \
		--push
