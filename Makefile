BINARY  := mailbox
PKG     := ./...
VERSION := 0.0.0
MONOVA  := $(shell which monova 2> /dev/null)
# version.go is package main, so the linker symbol is main.Version (not the full
# import path — the import-path form silently no-ops and leaves Version="dev").
LDFLAGS  = -ldflags="-X main.Version=$(VERSION)"
# RPM needs a concrete version even before any git tag exists.
RPMVERSION := $(shell monova 2>/dev/null || echo 0.0.0)

# Release artifacts (binary tarball + RPM) land here.
DIST   := dist
ARCH   := $(shell go env GOARCH)
RELDIR := $(BINARY)-$(RPMVERSION)-linux-$(ARCH)

export PATH := $(PATH):$(shell go env GOPATH)/bin
# This is a cgo/GTK app: it links against system GTK4/WebKit via pkg-config and
# cannot cross-compile trivially. Build Linux-only with cgo enabled.
export CGO_ENABLED := 1

version:
ifdef MONOVA
override VERSION = $(shell monova)
override LDFLAGS = -ldflags="-X main.Version=$(VERSION)"
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

# release-artifacts builds the stamped binary, a portable binary tarball, and the
# RPM into dist/. The binary links dynamically against system GTK4/WebKit, so the
# tarball still needs those libraries present (the RPM declares them as Requires);
# the tarball is a convenience, the RPM is the real install.
release-artifacts: build rpm
	rm -rf $(DIST)
	mkdir -p $(DIST)/$(RELDIR)
	cp bin/$(BINARY) packaging/com.jsnjack.mailbox.desktop packaging/com.jsnjack.mailbox.svg \
	    LICENSE README.md $(DIST)/$(RELDIR)/
	tar -C $(DIST) -czf $(DIST)/$(RELDIR).tar.gz $(RELDIR)
	rm -rf $(DIST)/$(RELDIR)
	@find rpmbuild/RPMS -name 'mailbox-$(RPMVERSION)-*.rpm' ! -name '*debug*' -exec cp {} $(DIST)/ \;
	@echo "==> release artifacts in $(DIST)/:"; ls -1 $(DIST)

# release builds the artifacts, then publishes a GitHub release tagged v<version>
# (monova) with grm, uploading the tarball + RPM. grm gets-or-creates the release
# (so a re-run resumes a partial upload) and authenticates with its configured
# token (`grm set token …`). Push the commit you want tagged first.
release: release-artifacts
	@command -v grm >/dev/null 2>&1 || { echo "grm required: grm install jsnjack/grm"; exit 1; }
	@args=""; for f in $(DIST)/*.tar.gz $(DIST)/*.rpm; do args="$$args -f $$f"; done; \
	  echo "==> grm release jsnjack/$(BINARY)$$args -t v$(RPMVERSION)"; \
	  grm release jsnjack/$(BINARY) $$args -t "v$(RPMVERSION)"
	@echo "==> released v$(RPMVERSION)"

clean:
	rm -rf bin/ rpmbuild/ $(DIST) $(BINARY)

.PHONY: version build run rpm test vet fmt lint check standards release release-artifacts clean
