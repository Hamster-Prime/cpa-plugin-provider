PLUGIN_ID := multi-protocol-provider
CMD       := ./cmd/$(PLUGIN_ID)
DIST      := dist

GO ?= go
HOST_GOOS := $(shell $(GO) env GOOS)
EXT := $(if $(filter windows,$(HOST_GOOS)),dll,$(if $(filter darwin,$(HOST_GOOS)),dylib,so))
OUTPUT := $(DIST)/$(PLUGIN_ID).$(EXT)

.PHONY: all build clean fmt test race vet

all: build

build:
	@mkdir -p $(DIST)
	CGO_ENABLED=1 $(GO) build -buildvcs=false -trimpath -buildmode=c-shared -ldflags="-s -w" -o $(OUTPUT) $(CMD)
	@rm -f $(DIST)/$(PLUGIN_ID).h

clean:
	rm -rf $(DIST)

fmt:
	$(GO) fmt ./...

test:
	$(GO) test ./... ./.github/scripts

race:
	$(GO) test -race ./... ./.github/scripts

vet:
	$(GO) vet ./... ./.github/scripts
