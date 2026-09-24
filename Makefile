# One definition of each check, used both locally and by CI (.github/workflows/ci.yml).
GO       ?= go
DIST     ?= dist
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X github.com/sentania-labs/benchwarmer/internal/version.Version=$(VERSION)

.PHONY: all fmt-check vet lint test test-race vuln windows check clean

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

clean:
	rm -rf $(DIST) $(BIN)
