SHELL := /bin/bash
.SHELLFLAGS := -e -o pipefail -c

GO ?= go
CARGO ?= cargo
REMOTE ?= comp-agent
REMOTE_DIR ?= agent-comp

BIN := bin
GOOS ?= linux
GOARCH ?= amd64
GUEST_TARGET ?= x86_64-unknown-linux-musl
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GO_LDFLAGS := -s -w -X github.com/mondeep31/agent-comp/internal/version.Version=$(VERSION)

.PHONY: all lint lint-go lint-rust lint-proto fmt test test-go test-rust build build-go build-agentd \
	proto integration bench audit image sync clean help

all: lint test build ## Lint, test and build everything

help: ## Show targets
	@grep -hE '^[a-zA-Z_%-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "%-16s %s\n", $$1, $$2}'

lint: lint-go lint-rust lint-proto ## Run all linters

lint-go:
	golangci-lint run ./...

lint-rust:
	cd guest && $(CARGO) fmt --all --check
	cd guest && $(CARGO) clippy --all-targets --locked -- -D warnings

lint-proto:
	buf lint
	buf format --diff --exit-code

fmt: ## Format Go, Rust and protobuf sources
	golangci-lint fmt ./...
	cd guest && $(CARGO) fmt --all
	buf format -w

test: test-go test-rust ## Run unit tests

test-go:
	$(GO) test -race -count=1 ./...

test-rust:
	cd guest && $(CARGO) test --locked

build: build-go build-agentd ## Build acd, acctl and agentd

build-go:
	mkdir -p $(BIN)
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build -trimpath -ldflags '$(GO_LDFLAGS)' -o $(BIN)/acd ./cmd/acd
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build -trimpath -ldflags '$(GO_LDFLAGS)' -o $(BIN)/acctl ./cmd/acctl

build-agentd:
	mkdir -p $(BIN)
	cd guest && $(CARGO) build --release --locked --target $(GUEST_TARGET)
	install -m 0755 guest/target/$(GUEST_TARGET)/release/agentd $(BIN)/agentd

proto: ## Regenerate Go code from proto/ (Rust generates at build time)
	buf generate

integration: ## KVM integration tests (Linux host with /dev/kvm; test binaries run under sudo)
	$(GO) test -exec 'sudo -E' -race -count=1 -tags integration -timeout 15m $(or $(PKG),./...) $(if $(RUN),-run '$(RUN)') -v

bench: build-agentd ## Golden, pool and end-to-end latency numbers on this host (docs/BENCHMARKS.md)
	$(MAKE) integration PKG='./internal/pool/ ./internal/daemon/' RUN='TestGoldenAndPool|TestEndToEnd'

audit: ## Vulnerability and license checks
	govulncheck ./...
	cd guest && $(CARGO) deny check
	cd guest && $(CARGO) audit

IMAGE_DIR ?= /var/lib/agent-comp/images

image: build-agentd ## Build the base guest image and install the kernel into IMAGE_DIR (sudo)
	sudo AGENTD=$(CURDIR)/$(BIN)/agentd ./images/build-base.sh $(IMAGE_DIR)
	sudo ./images/install-kernel.sh /var/lib/agent-comp/ci $(IMAGE_DIR)

sync: ## Copy the working tree to the dev host
	rsync -az --delete \
		--exclude .git --exclude /bin --exclude /guest/target --exclude .agents \
		./ $(REMOTE):$(REMOTE_DIR)/

remote-%: sync ## Run any target on the dev host, e.g. make remote-test
	ssh $(REMOTE) 'cd $(REMOTE_DIR) && bash -lc "make $* VERSION=$(VERSION)"'

clean:
	rm -rf $(BIN)
	cd guest && $(CARGO) clean
