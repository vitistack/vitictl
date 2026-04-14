BINARY := viti
PKG    := github.com/vitistack/vitictl

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

# Basic colors
BLACK=\033[0;30m
RED=\033[0;31m
GREEN=\033[0;32m
YELLOW=\033[0;33m
BLUE=\033[0;34m
PURPLE=\033[0;35m
CYAN=\033[0;36m
WHITE=\033[0;37m

# Text formatting
BOLD=\033[1m
UNDERLINE=\033[4m
RESET=\033[0m

# Cross-compile target; override to build release artifacts.
GOOS   ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS ?= -s -w -X main.version=$(VERSION)

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. Categories are represented by '##@' and target
# descriptions by '##'. See aks-operator/Makefile for the original.
.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: fix
fix: ## Run go fix against code.
	go fix ./...

.PHONY: test
test: fmt vet ## Run tests.
	go test ./... -coverprofile cover.out

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter.
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint and apply fixes.
	$(GOLANGCI_LINT) run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint configuration.
	$(GOLANGCI_LINT) config verify

##@ Build

.PHONY: build
build: fmt vet ## Build the viti binary into bin/$(BINARY).
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) .

.PHONY: run
run: ## Run viti from source (pass args via ARGS=...).
	go run . $(ARGS)

.PHONY: install
install: ## Install viti to $GOBIN.
	go install -ldflags "$(LDFLAGS)" .

.PHONY: clean
clean: ## Remove build artifacts.
	rm -rf bin/ cover.out

##@ Security

.PHONY: gosec
gosec: install-security-scanner ## Run gosec security scan (fails on findings).
	$(GOSEC) ./...

.PHONY: govulncheck
govulncheck: install-govulncheck ## Run govulncheck vulnerability scan (fails on findings).
	$(GOVULNCHECK) ./...

##@ SBOM (Software Bill of Materials)
SYFT ?= $(LOCALBIN)/syft
SYFT_VERSION ?= latest
SBOM_OUTPUT_DIR ?= sbom
SBOM_PROJECT_NAME ?= viti
SBOM_VERSION ?= $(VERSION)

.PHONY: install-syft
install-syft: $(SYFT) ## Install syft SBOM generator locally.
$(SYFT): $(LOCALBIN)
	@set -e; echo "Installing syft $(SYFT_VERSION)"; \
	curl -sSfL https://raw.githubusercontent.com/anchore/syft/main/install.sh | sh -s -- -b $(LOCALBIN)

.PHONY: sbom
sbom: install-syft ## Generate SBOMs for Go source (CycloneDX + SPDX).
	@mkdir -p $(SBOM_OUTPUT_DIR)
	@echo "Downloading Go modules for license detection..."
	go mod download
	@echo "Generating source code SBOMs..."
	$(SYFT) dir:. --source-name=$(SBOM_PROJECT_NAME) --source-version=$(SBOM_VERSION) -o cyclonedx-json=$(SBOM_OUTPUT_DIR)/sbom-source.cdx.json
	$(SYFT) dir:. --source-name=$(SBOM_PROJECT_NAME) --source-version=$(SBOM_VERSION) -o spdx-json=$(SBOM_OUTPUT_DIR)/sbom-source.spdx.json
	@echo "SBOMs generated: $(SBOM_OUTPUT_DIR)/sbom-source.{cdx,spdx}.json"

##@ Dependencies

.PHONY: deps
deps: ## Download and verify dependencies.
	@echo -e "Downloading dependencies..."
	@go mod download
	@go mod verify
	@go mod tidy
	@echo -e "Dependencies updated!"

.PHONY: update-deps
update-deps: ## Update dependencies.
	@echo -e "Updating dependencies..."
	@go get -u ./...
	@go mod tidy
	@echo -e "Dependencies updated!"

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
GOSEC        ?= $(LOCALBIN)/gosec
GOVULNCHECK  ?= $(LOCALBIN)/govulncheck

## Tool Versions
GOLANGCI_LINT_VERSION ?= latest
GOSEC_VERSION         ?= latest
GOVULNCHECK_VERSION   ?= latest

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Install golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

.PHONY: install-security-scanner
install-security-scanner: $(GOSEC) ## Install gosec security scanner locally.
$(GOSEC): $(LOCALBIN)
	@set -e; echo "Attempting to install gosec $(GOSEC_VERSION)"; \
	if ! GOBIN=$(LOCALBIN) go install github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VERSION) 2>/dev/null; then \
		echo "Primary install failed, attempting install from @main (compatibility fallback)"; \
		if ! GOBIN=$(LOCALBIN) go install github.com/securego/gosec/v2/cmd/gosec@main; then \
			echo "gosec installation failed for versions $(GOSEC_VERSION) and @main"; \
			exit 1; \
		fi; \
	fi; \
	echo "gosec installed at $(GOSEC)"; \
	chmod +x $(GOSEC)

.PHONY: install-govulncheck
install-govulncheck: $(GOVULNCHECK) ## Install govulncheck locally.
$(GOVULNCHECK): $(LOCALBIN)
	@set -e; echo "Attempting to install govulncheck $(GOVULNCHECK_VERSION)"; \
	if ! GOBIN=$(LOCALBIN) go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) 2>/dev/null; then \
		echo "Primary install failed, attempting install from @latest (compatibility fallback)"; \
		if ! GOBIN=$(LOCALBIN) go install golang.org/x/vuln/cmd/govulncheck@latest; then \
			echo "govulncheck installation failed for versions $(GOVULNCHECK_VERSION) and @latest"; \
			exit 1; \
		fi; \
	fi; \
	echo "govulncheck installed at $(GOVULNCHECK)"; \
	chmod +x $(GOVULNCHECK)

# go-install-tool will 'go install' any package with custom target and name of
# binary, if it doesn't exist.
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $$(realpath $(1)-$(3)) $(1)
endef
