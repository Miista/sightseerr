BINARY := sightseerr

REGISTRY ?= localhost:5000
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
ARCH     ?= amd64

.PHONY: all build image test cover clean dist push

all: build

build:
	go build -trimpath -o $(BINARY) .

# Builds the image from the cross-compiled binary; the image never compiles anything.
image: dist
	docker build --platform linux/$(ARCH) --build-arg TARGETARCH=$(ARCH) -t $(BINARY):dev .

# -race catches data races; -shuffle=on catches tests that depend on each other's state
# (Go prints the seed, and -shuffle=<seed> replays the order).
test:
	go test -race -shuffle=on ./...

cover:
	go test -race -shuffle=on -coverprofile=.coverage -covermode=atomic ./... >/dev/null
	go tool cover -func=.coverage | tail -1

clean:
	rm -f $(BINARY) .coverage
	rm -rf dist

# Cross-compile with Go (no QEMU, no compiling inside Docker).
dist:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -trimpath \
	  -ldflags "-s -w -X main.version=$(VERSION)" -o dist/linux/$(ARCH)/$(BINARY) .

push: image
	docker tag $(BINARY):dev $(REGISTRY)/$(BINARY):$(VERSION)
	docker tag $(BINARY):dev $(REGISTRY)/$(BINARY):latest
	docker push $(REGISTRY)/$(BINARY):$(VERSION)
	docker push $(REGISTRY)/$(BINARY):latest
