BINARY  := mailbox
PKG     := ./...
VERSION := 0.0.0
MONOVA  := $(shell which monova 2> /dev/null)
PKGPATH := github.com/jsnjack/mailbox/cmd/mailbox
LDFLAGS  = -ldflags="-X $(PKGPATH).Version=$(VERSION)"
# RPM needs a concrete version even before any git tag exists.
RPMVERSION := $(shell monova 2>/dev/null || echo 0.0.0)

export PATH := $(PATH):$(shell go env GOPATH)/bin
# This is a cgo/GTK app: it links against system GTK4/WebKit via pkg-config and
# cannot cross-compile trivially. Build Linux-only with cgo enabled.
export CGO_ENABLED := 1

version:
ifdef MONOVA
override VERSION = $(shell monova)
override LDFLAGS = -ldflags="-X $(PKGPATH).Version=$(VERSION)"
else
$(info "Install monova with: grm install jsnjack/monova")
endif

test:
	go test $(PKG)

vet:
	go vet $(PKG)

fmt:
	@command -v goimports >/dev/null 2>&1 || { \
	  echo "goimports is not installed. Install it with:"; \
	  echo "  go install golang.org/x/tools/cmd/goimports@latest"; \
	  exit 1; \
	}
	goimports -w .

lint: vet
	@command -v golangci-lint >/dev/null 2>&1 || { \
	  echo "golangci-lint is not installed. Install it with:"; \
	  echo "  grm install golangci/golangci-lint"; \
	  exit 1; \
	}
	golangci-lint run

check: fmt vet build test lint
	@echo "==> make check: all green"

standards:
	curl -sL https://raw.githubusercontent.com/jsnjack/standards/master/AGENTS.universal.md \
	    -o AGENTS.universal.md
	curl -sL https://raw.githubusercontent.com/jsnjack/standards/master/AGENTS.go.md \
	    -o AGENTS.go.md

bin/$(BINARY): version
	go build $(LDFLAGS) -o bin/$(BINARY) ./cmd/mailbox
	ln -sf bin/$(BINARY) $(BINARY)

build: bin/$(BINARY)

run: build
	./bin/$(BINARY)

rpm: packaging/mailbox.spec build
	rpmbuild -bb --define "_topdir $(CURDIR)/rpmbuild" \
	    --define "srcdir $(CURDIR)" \
	    --define "appversion $(RPMVERSION)" packaging/mailbox.spec
	@echo "==> RPM(s):"; find rpmbuild/RPMS -name '*.rpm' 2>/dev/null

clean:
	rm -rf bin/ rpmbuild/ $(BINARY)

.PHONY: version build run rpm test vet fmt lint check standards clean
