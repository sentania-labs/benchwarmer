# One definition of each check, used both locally and by CI (.github/workflows/ci.yml).
GO       ?= go
DIST     ?= dist
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X github.com/sentania-labs/benchwarmer/internal/version.Version=$(VERSION)

.PHONY: all fmt-check vet lint test test-race vuln windows check stage package clean

all: check

# Only this module's packages (not agent worktrees or other nested checkouts).
fmt-check:
	@out=$$(gofmt -l $$($(GO) list -f '{{.Dir}}' ./...)); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...
	GOOS=windows $(GO) vet ./...

# Build staticcheck for the host once, then analyse both target platforms.
BIN := .bin
$(BIN)/staticcheck: go.mod go.sum
	mkdir -p $(BIN)
	$(GO) build -o $@ honnef.co/go/tools/cmd/staticcheck

lint: fmt-check vet $(BIN)/staticcheck
	$(BIN)/staticcheck ./...
	GOOS=windows $(BIN)/staticcheck ./...

test:
	$(GO) test -count=1 ./...

test-race:
	$(GO) test -count=1 -race ./...

vuln:
	$(GO) tool govulncheck ./...

windows:
	mkdir -p $(DIST)
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/bwprobe.exe ./cmd/bwprobe
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/fakellama.exe ./cmd/fakellama
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS) -H windowsgui" -o $(DIST)/bwtray.exe ./cmd/bwtray

check: lint test-race vuln windows

# Release file set (ADR 0012): what the MSI and the zip both install under
# C:\Program Files\Benchwarmer\. The llama.cpp runtime is the release pinned
# in packaging/llama-cpp.json, verified by SHA-256.
STAGE    ?= $(DIST)/stage
PKG      := benchwarmer-$(VERSION)-windows-amd64
stage:
	rm -rf $(STAGE)
	mkdir -p $(STAGE)/licenses
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(STAGE)/benchwarmer.exe ./cmd/benchwarmer
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS) -H windowsgui" -o $(STAGE)/bwtray.exe ./cmd/bwtray
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(STAGE)/bwprobe.exe ./cmd/bwprobe
	packaging/fetch-llama-cpp.sh $(STAGE)
	if [ -f LICENSE ]; then cp LICENSE $(STAGE)/licenses/LICENSE-benchwarmer; fi
	$(GO) run ./cmd/benchwarmer config default > $(STAGE)/config.example.json
	printf '%s\n' '$(VERSION)' > $(STAGE)/VERSION

# The by-hand zip: the staged file set plus install.ps1 / uninstall.ps1.
package: stage
	rm -rf $(DIST)/$(PKG) $(DIST)/$(PKG).zip
	cp -r $(STAGE) $(DIST)/$(PKG)
	cp installer/install.ps1 installer/uninstall.ps1 $(DIST)/$(PKG)/
	cd $(DIST) && zip -qr $(PKG).zip $(PKG)

clean:
	rm -rf $(DIST) $(BIN) .cache
