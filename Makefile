# Stacks for Push 3. Push is x86-64 Linux with glibc 2.35, so the cgo
# binary is linked STATICALLY: a dynamic one built on a newer glibc fails
# to exec on the device. Everything runs in an amd64 Go container.
#
# An optional go.work (local, not committed) points core at ../ableton-push-hack/core;
# both repos are mounted so it resolves inside the container.

IMAGE   ?= golang:1.25
HOST    ?= push.local
DOCKER  := docker run --rm --platform linux/amd64 \
	-v $(CURDIR):/w/push-hack-stacks \
	-v $(abspath $(CURDIR)/../ableton-push-hack):/w/ableton-push-hack \
	-v push-hack-stacks-gomod:/go/pkg/mod \
	-w /w/push-hack-stacks/src -e CGO_ENABLED=1 -e GOFLAGS=-buildvcs=false $(IMAGE)
LDFLAGS := -s -w -linkmode external -extldflags "-static"

.PHONY: all build test vet deploy clean matrix

all: build

build:
	$(DOCKER) go build -trimpath -tags netgo,osusergo -ldflags '$(LDFLAGS)' -o ../stacks .

test:
	$(DOCKER) go test ./...

vet:
	$(DOCKER) go vet ./...

deploy: build
	./scripts/deploy.sh $(HOST)

clean:
	rm -f stacks

# docs/control-matrix.html, from the code: cmd/matrixdata exports labels,
# pad rows and screen renders; scripts/matrix/build.py lays them out with
# the Move matrix's stylesheet and knob texts (../schwung-stacks).
STACKS_REPO ?= $(abspath $(CURDIR)/../schwung-stacks)
matrix:
	$(DOCKER) go run ./cmd/matrixdata -state ../scripts/matrix/axis-state.json -out ../build/matrix
	python3 -I scripts/matrix/build.py build/matrix $(STACKS_REPO)/docs/stacks-matrix.html \
		docs/control-matrix.html $$(git rev-parse --short HEAD)
