# Terraform provider for k0rdent AI.
#
# Target names follow the KCM provider's Makefile (build, install, testacc,
# fmt, lint, docs, mod), the convention worth taking from it.

# ---------------------------------------------------------------------------
# The provisional name: the non-Go half of the single-constant rule.
#
# PROVIDER_NAME must stay identical to ProviderTypeName in
# internal/provider/names.go, and NAMESPACE to RegistryNamespace. `make
# name-audit` fails when they drift, and prints every other place the
# provisional name appears. Settling the name is an edit to these two
# variables plus the two Go constants.
# ---------------------------------------------------------------------------
PROVIDER_NAME ?= k0rdent
NAMESPACE     ?= k0rdent
REGISTRY      ?= registry.terraform.io
VERSION       ?= 0.1.0

BINARY  := terraform-provider-$(PROVIDER_NAME)
OS_ARCH := $(shell go env GOOS)_$(shell go env GOARCH)
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

GOLANGCI_LINT_VERSION ?= v2.13.2
GORELEASER_VERSION    ?= v2.18.2
TFPLUGINDOCS_VERSION  ?= v0.25.0
GOVULNCHECK_VERSION   ?= v1.8.0

# The Go the signed release is built with. release-dispatch.yml, the docs job
# and the release rehearsal in ci.yml read the same file through setup-go's
# go-version-file, so raising it is a one-line change that the build and the
# vulnerability gate both see.
RELEASE_GO_VERSION ?= $(shell cat .go-release-version)

# golangci-lint refuses to load .golangci.yml when the binary was built with a
# Go older than this module's `go` directive. `go install` without GOTOOLCHAIN
# can produce that binary even when the module itself builds (Go will download
# a newer toolchain for the module, then still compile the linter with the
# older local one). Pin the install to go.mod so `make lint-install` is enough.
MODULE_GO_VERSION ?= $(shell awk '/^go / {print $$2; exit}' go.mod)
GOLANGCI_LINT_GOTOOLCHAIN ?= go$(MODULE_GO_VERSION)

TOOLS_BIN      ?= $(CURDIR)/.tools/bin
GORELEASER     ?= $(TOOLS_BIN)/goreleaser
TFPLUGINDOCS   ?= $(TOOLS_BIN)/tfplugindocs
TF_BIN         ?= terraform
TF_REGISTRY_HOST ?= registry.terraform.io

.DEFAULT_GOAL := check

.PHONY: help
help: ## List targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: check
check: doc-commands fmt-check vet lint test name-audit client-check docs-check vulncheck ## Everything CI runs

.PHONY: vulncheck
vulncheck: ## Gate S6: reachable known vulnerabilities, under the CI Go and the release Go
	# Reachability-based rather than a generic scanner. A scanner that reports
	# the whole module graph produces a list people route around; govulncheck
	# reports only what this code actually calls, so a hit is a thing to fix.
	# It needs the network for the vulnerability database.
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...
	# Again under the release Go. Standard library findings belong to the
	# toolchain govulncheck runs under, so the line above can be clean while
	# the Go that builds the signed binaries has advisories. That happened
	# with go1.27.1, which CI never scanned. GOTOOLCHAIN downloads it if
	# it isn't installed.
	GOTOOLCHAIN=go$(RELEASE_GO_VERSION) go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

.PHONY: build
build: ## Compile the provider binary
	go build $(LDFLAGS) -o $(BINARY) .

# Every platform .goreleaser.yaml ships, so a break that only shows on one of
# them fails a pull request without the full release rehearsal.
RELEASE_PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64

.PHONY: cross-build
cross-build: ## Compile for every release platform without building archives
	@for p in $(RELEASE_PLATFORMS); do \
		echo "go build $$p"; \
		CGO_ENABLED=0 GOOS=$${p%/*} GOARCH=$${p#*/} go build -o /dev/null . || exit 1; \
	done

.PHONY: install
install: build ## Install into the local plugin mirror for dev_overrides
	mkdir -p ~/.terraform.d/plugins/$(REGISTRY)/$(NAMESPACE)/$(PROVIDER_NAME)/$(VERSION)/$(OS_ARCH)
	mv $(BINARY) ~/.terraform.d/plugins/$(REGISTRY)/$(NAMESPACE)/$(PROVIDER_NAME)/$(VERSION)/$(OS_ARCH)

.PHONY: test
test: ## Unit tests
	go test -race -count=1 ./...

.PHONY: vet
vet: ## go vet
	go vet ./...

# The module's tracked sources, so `make fmt` and `make fmt-check` read the
# same files.
GO_SOURCE_FILES := $(shell git ls-files '*.go')

.PHONY: fmt
fmt: ## Format Go sources
	@if [ -n "$(GO_SOURCE_FILES)" ]; then gofmt -w $(GO_SOURCE_FILES); fi

.PHONY: fmt-check
fmt-check: ## Fail when sources are unformatted
	@unformatted=$$(if [ -n "$(GO_SOURCE_FILES)" ]; then gofmt -l $(GO_SOURCE_FILES); fi); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed:"; echo "$$unformatted"; exit 1; \
	fi

.PHONY: lint
lint: ## golangci-lint
	golangci-lint run ./...

.PHONY: lint-install
lint-install: ## Install the pinned golangci-lint, built with this module's Go version
	GOTOOLCHAIN=$(GOLANGCI_LINT_GOTOOLCHAIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: tools
tools: $(TFPLUGINDOCS) ## Install the pinned docs generator

$(TFPLUGINDOCS):
	mkdir -p $(TOOLS_BIN)
	GOBIN="$(TOOLS_BIN)" go install github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs@$(TFPLUGINDOCS_VERSION)

.PHONY: docs
docs: $(TFPLUGINDOCS) ## Generate committed provider registry documentation
	TF_BIN="$(TF_BIN)" TF_REGISTRY_HOST="$(TF_REGISTRY_HOST)" \
		PROVIDER_NAME="$(PROVIDER_NAME)" ./scripts/generate-provider-docs.sh "$(TFPLUGINDOCS)"

.PHONY: doc-commands
doc-commands: ## Refuse a # comment in a Markdown shell block, which breaks when pasted into zsh
	./scripts/check-doc-commands.sh

.PHONY: doc-commands-test
doc-commands-test: ## Prove doc-commands fails on each kind of comment and passes clean blocks
	./scripts/tests/test-doc-commands.sh

.PHONY: docs-check
docs-check: $(TFPLUGINDOCS) ## Fail when generated provider documentation is stale
	@$(MAKE) --no-print-directory docs; \
	if ! git diff --quiet -- docs templates || \
		[ -n "$$(git ls-files --others --exclude-standard -- docs templates)" ]; then \
		echo "generated provider documentation is stale; run 'make docs' and commit docs/ and templates/"; \
		git status --short -- docs templates; \
		exit 1; \
	fi

.PHONY: mod
mod: ## Download module dependencies that can actually be resolved
	go build ./...

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum
	go mod tidy

# ---------------------------------------------------------------------------
# The API client in internal/k0rdent, generated from spec/openapi.yaml there.
# That spec is k0rdent-clients' prepared spec cut to the operations in
# spec/operations.txt. See internal/k0rdent/README.md.
# ---------------------------------------------------------------------------
CLIENT_DIR       := internal/k0rdent
CLIENT_GENERATED := $(CLIENT_DIR)/types.gen.go $(CLIENT_DIR)/client.gen.go \
                    $(CLIENT_DIR)/contract.gen.go $(CLIENT_DIR)/limits.gen.go
CLIENTS_GO       ?= ../k0rdent-clients/go
CODEGEN_MODULE   := github.com/oapi-codegen/oapi-codegen/v2

.PHONY: client-generate
client-generate: ## Regenerate the API client from internal/k0rdent/spec
	go generate ./$(CLIENT_DIR)

.PHONY: client-check
client-check: client-template-check ## Fail if the generated API client differs from a fresh generate
	@$(MAKE) --no-print-directory client-generate >/dev/null
	@if ! git diff --exit-code -- $(CLIENT_GENERATED); then \
		echo; \
		echo "ERROR: the generated client is stale. Run 'make client-generate' and commit the result."; \
		echo "If the spec didn't change, the generator did: check its version in go.mod."; \
		exit 1; \
	fi
	@untracked=$$(git ls-files --others --exclude-standard -- '$(CLIENT_DIR)/*.gen.go'); \
	if [ -n "$$untracked" ]; then \
		echo "ERROR: generate wrote files that aren't committed:"; echo "$$untracked"; \
		echo "Add them to CLIENT_GENERATED in the Makefile and commit them."; \
		exit 1; \
	fi
	@echo "the generated client is up to date"

# Every templates/<name>.tmpl overrides the generator's stock <name>.tmpl, and
# a generator upgrade can change the stock one without client-check noticing:
# the override keeps emitting the old shape. The <name>.stock.tmpl beside each
# override is the stock template it was made from, so this diffs it against
# the pinned generator's own copy. templates/added/ holds templates with no
# stock counterpart, and each fails here if the generator gains one of its name.
.PHONY: client-template-check
client-template-check: ## Fail if a vendored generator template drifted from the pinned generator
	@go mod download $(CODEGEN_MODULE) || exit 1; \
	dir=$$(go list -m -f '{{.Dir}}' $(CODEGEN_MODULE)); \
	[ -n "$$dir" ] || { echo "ERROR: no module directory for $(CODEGEN_MODULE)."; exit 1; }; \
	cd $(CLIENT_DIR) || exit 1; \
	fail=0; n=0; \
	for override in templates/*.tmpl; do \
		[ -e "$$override" ] || continue; \
		case "$$override" in *.stock.tmpl) continue ;; esac; \
		n=$$((n + 1)); \
		name=$$(basename "$$override"); \
		copy="templates/$${name%.tmpl}.stock.tmpl"; \
		stock="$$dir/pkg/codegen/templates/$$name"; \
		if [ ! -f "$$copy" ]; then \
			echo "ERROR: $$override has no stock copy at $$copy. See $(CLIENT_DIR)/templates/README.md."; \
			fail=1; continue; \
		fi; \
		if ! grep -qE "^[[:space:]]+$$name: $$override\$$" oapi-codegen.*.yaml; then \
			echo "ERROR: no oapi-codegen.*.yaml points user-templates at $$override, so the generator never reads it."; \
			fail=1; \
		fi; \
		if [ ! -f "$$stock" ]; then \
			echo "ERROR: $(CODEGEN_MODULE) no longer has $$stock. Find where it went and update templates/."; \
			fail=1; continue; \
		fi; \
		if ! diff -u "$$stock" "$$copy"; then \
			echo "ERROR: $$copy is not the pinned generator's template. Copy the new stock template"; \
			echo "over it, re-apply the changes to $$override, then 'make client-generate' and read the diff."; \
			fail=1; \
		fi; \
	done; \
	for copy in templates/*.stock.tmpl; do \
		[ -e "$$copy" ] || continue; \
		if [ ! -f "$${copy%.stock.tmpl}.tmpl" ]; then \
			echo "ERROR: $$copy has no override beside it. Delete it or add the override."; \
			fail=1; \
		fi; \
	done; \
	for added in templates/added/*.tmpl; do \
		[ -e "$$added" ] || continue; \
		if [ -f "$$dir/pkg/codegen/templates/$$(basename "$$added")" ]; then \
			echo "ERROR: $$added has the name of a stock template, so it overrides it with no stock copy to diff."; \
			fail=1; \
		fi; \
	done; \
	[ "$$fail" -eq 0 ] || exit 1; \
	echo "$$n vendored template(s) match the pinned generator"

# Needs a k0rdent-clients checkout, so CI never runs it. CLIENTS_GO is its go/
# directory. The checkout has to be clean, because SOURCE.json records its
# commit as the source.
.PHONY: client-sync
client-sync: ## Re-cut the client's spec from a k0rdent-clients checkout (CLIENTS_GO), then regenerate
	@[ -f "$(CLIENTS_GO)/spec/prepared/openapi.yaml" ] || { echo "ERROR: no prepared spec under CLIENTS_GO=$(CLIENTS_GO)."; exit 1; }
	@[ -z "$$(git -C "$(CLIENTS_GO)" status --porcelain)" ] || { echo "ERROR: $(CLIENTS_GO) has uncommitted changes."; exit 1; }
	cd $(CLIENT_DIR) && go run ../tools/clientspec -from "$(abspath $(CLIENTS_GO))" \
		-commit "$$(git -C "$(abspath $(CLIENTS_GO))" rev-parse HEAD)"
	@$(MAKE) --no-print-directory client-generate

.PHONY: name-audit
name-audit: ## Print every place the provisional provider name is written
	@go run ./internal/tools/nameaudit

.PHONY: clean
clean: ## Remove build output
	rm -f $(BINARY)
	rm -rf .tools
