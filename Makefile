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
check: hygiene public-text-check secrets-check doc-commands fmt-check vet lint test name-audit client-check docs-check vulncheck changelog-check ## Everything CI runs

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

.PHONY: testacc
testacc: ## Acceptance tests: each resource's own lifecycle against the local fake
	TF_ACC=1 TF_ACC_TERRAFORM_PATH="$$(command -v $(TF_BIN))" go test ./internal/acceptance/ -count=1 -timeout 20m

# The perpetual-diff check on its own, so the nightly can name it when it
# fails: every managed resource applied, then planned, refreshed and planned
# again, against a fake that varies what the platform varies between reads.
# `make testacc` runs it too.
.PHONY: drift
drift: ## The perpetual-diff check alone: every resource, plan and refresh against a fake that moves what the platform moves
	TF_ACC=1 TF_ACC_TERRAFORM_PATH="$$(command -v $(TF_BIN))" go test ./internal/acceptance/ -count=1 -timeout 20m -v \
		-run '^(TestAccPerpetualDiff|TestPerpetualDiffCoversEveryResource)$$'

# The performance benchmark: plan, refresh-only plan, apply and destroy at each
# size in BENCH_SIZES and each -parallelism in BENCH_PARALLELISM, against a
# fake of its own on a free port with BENCH_LATENCY added to every request.
# docs/performance.md is the method and the published figures. With
# BENCH_BUDGET it fails when a figure breaks that file's limits, which is
# how the nightly holds the published figures to account. Results land in
# .bench/.
BENCH_SIZES       ?= 10,100,500,1000
BENCH_PARALLELISM ?= 10,4,32
BENCH_LATENCY     ?= 3ms
BENCH_BUDGET      ?=
.PHONY: bench
bench: ## Time plan, refresh and apply at 10 to 1000 resources against the fake. BENCH_BUDGET=<file> gates
	go run ./internal/tools/bench -sizes "$(BENCH_SIZES)" -parallelism "$(BENCH_PARALLELISM)" \
		-latency "$(BENCH_LATENCY)" -terraform "$(TF_BIN)" -out .bench \
		$(if $(BENCH_BUDGET),-budget "$(BENCH_BUDGET)")

# The sweepers delete what a killed acceptance run left on a real API: every
# object whose id starts with `acc-`, and nothing else, children first. The
# endpoint, project and credential come from the same environment the
# provider reads. SWEEP_CONFIRM has to name the endpoint's
# host, as LIVE_CONFIRM does for `make live`. `-run '^$$'` is belt and braces:
# a sweep runs no test.
SWEEP_REGION ?=
.PHONY: sweep
sweep: ## Delete leaked acc- test objects. Needs SWEEP_REGION and SWEEP_CONFIRM=<endpoint host>
	@test -n "$(SWEEP_REGION)" || { echo "make sweep needs SWEEP_REGION, the region (or comma-separated regions) to sweep"; exit 2; }
	go test ./internal/acceptance/ -count=1 -timeout 60m -run '^$$' -v -sweep="$(SWEEP_REGION)"

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: demo
demo: ## The whole provider working end to end against a local fake. Needs nothing secret
	./scripts/demo.sh

.PHONY: demo-up
demo-up: ## Start the fake and build the provider, then leave both up for hand-editing
	./scripts/demo.sh up

.PHONY: demo-down
demo-down: ## Stop a fake left running by demo-up
	./scripts/demo.sh down

.PHONY: demo-env
demo-env: ## Print the exports a new shell needs to drive examples/demo by hand
	@./scripts/demo.sh env

.PHONY: demo-session
demo-session: ## Keep a fake and interactive shell together for tool runners
	./scripts/demo.sh session

.PHONY: plan-fake
plan-fake: ## The configuration-layer harness: authentication and error paths only
	./ci/plan-against-fake.sh

.PHONY: walkthrough-test
walkthrough-test: ## Follow docs/walkthrough-hardware.md against the fake, with the directories in examples/walkthrough
	./ci/walkthrough-against-fake.sh

# Each walkthrough directory against the fake: a refresh-only plan after the
# apply finds nothing, and its plans' "known after apply" values stay within
# ci/unknown-budget.json. TESTING.md has how to update the budget.
.PHONY: plan-quality
plan-quality: ## Refresh-only plans find nothing, and plans stay within ci/unknown-budget.json, for every walkthrough directory
	./ci/plan-quality-against-fake.sh

.PHONY: unknown-budget-update
unknown-budget-update: ## Rewrite ci/unknown-budget.json from the walkthrough plans, keeping its reasons
	UNKNOWN_BUDGET_WRITE=1 ./ci/plan-quality-against-fake.sh

.PHONY: walkthrough-api-test
walkthrough-api-test: ## Follow docs/walkthrough-api.md against the fake, from the commands the page shows
	./ci/walkthrough-api-against-fake.sh

.PHONY: doctor
doctor: ## Check this machine can run the tests, before anything long fails halfway
	./scripts/doctor.sh

# `live` targets talk to a real control plane, configured entirely from the
# K0RDENT_* environment. `live-preflight` is read-only and safe anywhere;
# `live` creates real resources and requires LIVE_CONFIRM=<endpoint host>.
.PHONY: live-preflight
live-preflight: ## Read-only check against a real API: mint, list regions, list cluster types, plan
	./scripts/live.sh preflight

.PHONY: live
live: ## The full lifecycle against a real API. Creates real resources. Needs LIVE_CONFIRM
	./scripts/live.sh run

# A volume claim binds through a cluster's own storage class, reached through
# the kubeconfig the provider hands over. It needs a real deployment, since a
# MOCK_MODE kind stack has no cluster behind its kubeconfig.
# The validate target needs no endpoint, only the registry for the kubernetes
# provider.
.PHONY: live-storage
live-storage: ## A claim binds through the handed-over kubeconfig on a real deployment. Creates real resources. Needs LIVE_CONFIRM
	./scripts/live-storage.sh run

.PHONY: live-storage-validate
live-storage-validate: ## Init and validate examples/live/storage. Creates nothing
	./scripts/live-storage.sh validate

.PHONY: doctor-script-test
doctor-script-test: ## Verify doctor rejects golangci-lint v1 and a too-old linter Go
	./scripts/tests/test-doctor.sh

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
tools: $(GORELEASER) $(TFPLUGINDOCS) ## Install pinned release and docs tools

$(GORELEASER):
	mkdir -p $(TOOLS_BIN)
	GOBIN="$(TOOLS_BIN)" go install github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION)

$(TFPLUGINDOCS):
	mkdir -p $(TOOLS_BIN)
	GOBIN="$(TOOLS_BIN)" go install github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs@$(TFPLUGINDOCS_VERSION)

.PHONY: docs
docs: $(TFPLUGINDOCS) ## Generate committed provider registry documentation
	TF_BIN="$(TF_BIN)" TF_REGISTRY_HOST="$(TF_REGISTRY_HOST)" \
		PROVIDER_NAME="$(PROVIDER_NAME)" ./scripts/generate-provider-docs.sh "$(TFPLUGINDOCS)"

.PHONY: hygiene
hygiene: ## Refuse a tree that has grown a cache, an archive or a build output
	./scripts/repo-hygiene.sh

# The text a public repository mustn't hold, from .github/public-text/. The
# pre-commit hook runs the same two checks on what a commit adds, and the
# commit-msg hook runs the first on its message. `make hooks` sets
# core.hooksPath in the clone's config, so every worktree of the clone
# runs the hooks its own branch carries, and a branch without them runs none.
.PHONY: public-text-check
public-text-check: ## Refuse ticket keys, internal links, people and other text that isn't for a public repo
	./scripts/check-public-text.sh

.PHONY: public-text-test
public-text-test: ## Prove public-text-check fails on a planted hit, passes an allowlisted one and exits non-zero when it can't run
	./scripts/tests/test-public-text.sh

.PHONY: secrets-check
secrets-check: ## Scan the tracked files for secrets with the pinned gitleaks
	./scripts/check-secrets.sh

.PHONY: secrets-test
secrets-test: ## Prove secrets-check fails on a planted token and passes the fake's credentials
	./scripts/tests/test-secrets.sh

.PHONY: hooks
hooks: ## Opt in to the pre-commit and commit-msg hooks in scripts/hooks for this clone
	git config core.hooksPath scripts/hooks
	@echo "pre-commit and commit-msg hooks on. 'git config --unset core.hooksPath' turns it off."

.PHONY: doc-commands
doc-commands: ## Refuse a # comment in a Markdown shell block, which breaks when pasted into zsh
	./scripts/check-doc-commands.sh

.PHONY: doc-commands-test
doc-commands-test: ## Prove doc-commands fails on each kind of comment and passes clean blocks
	./scripts/tests/test-doc-commands.sh

.PHONY: hygiene-test
hygiene-test: ## Prove each hygiene gate still fires
	./scripts/tests/test-repo-hygiene.sh

.PHONY: script-credentials-test
script-credentials-test: ## Prove the live scripts keep credentials on https and off the command line
	./scripts/tests/test-script-credentials.sh

.PHONY: live-storage-script-test
live-storage-script-test: ## Prove make live-storage keeps KUBE_* away from Terraform and destroys a cluster whose kubeconfig won't open
	./scripts/tests/test-live-storage.sh

.PHONY: live-kept-vars-test
live-kept-vars-test: ## Prove LIVE_KEEP=1 keeps the cluster type and version, and a failed write doesn't cut the cleanup short
	./scripts/tests/test-live-kept-vars.sh

.PHONY: script-lib-test
script-lib-test: ## Test the helpers every script sources from scripts/lib
	./scripts/tests/test-script-lib.sh

# Every tracked script under scripts/ and ci/, at warning and above. Notes are
# left out because most are single quotes around text another program
# expands, which is the point. .shellcheckrc lets -x follow `. lib/common.sh`
# relative to the script that sources it. Not part of `check`, because
# shellcheck isn't a tool every machine has. CI runs it.
SHELL_SCRIPTS = $(shell git ls-files 'scripts/*.sh' 'scripts/hooks/*' 'ci/*.sh' 'examples/walkthrough/*.sh')

.PHONY: shellcheck
shellcheck: ## Shellcheck every script under scripts/, ci/ and examples/walkthrough/
	shellcheck -x -S warning $(SHELL_SCRIPTS)

.PHONY: docs-check
docs-check: $(TFPLUGINDOCS) ## Fail when generated provider documentation is stale
	@$(MAKE) --no-print-directory docs; \
	if ! git diff --quiet -- docs templates || \
		[ -n "$$(git ls-files --others --exclude-standard -- docs templates)" ]; then \
		echo "generated provider documentation is stale; run 'make docs' and commit docs/ and templates/"; \
		git status --short -- docs templates; \
		exit 1; \
	fi

# Release notes are one .changelog/<pull request>.txt per pull request, in
# go-changelog's block format, and .changelog/README.md is how to write one.
# The check refuses a malformed entry anywhere in the directory, and that's
# all it does locally. With CHANGELOG_PR and CHANGELOG_BASE set, as
# .github/workflows/changelog.yml sets them from the pull request event, it
# also diffs the directory against the base and requires the pull request's
# own entry, unless CHANGELOG_LABELS holds no-changelog-needed. The tool is in
# this module and needs only the standard library, so there's nothing to pin
# and the workflow needs no client checkout.
CHANGELOG_REPO ?= https://github.com/k0rdent/terraform-provider-$(PROVIDER_NAME)
CHANGELOG_DATE ?=

.PHONY: changelog-check
changelog-check: ## Refuse a malformed .changelog/ entry. With CHANGELOG_PR and CHANGELOG_BASE, judge that pull request's changes too
	go run ./internal/tools/changelog -check -prefix $(PROVIDER_NAME)_

.PHONY: changelog
changelog: ## Print .changelog/ as a CHANGELOG.md section. VERSION=x.y.z, and CHANGELOG_DATE=YYYY-MM-DD for a release
	@go run ./internal/tools/changelog -prefix $(PROVIDER_NAME)_ -version "$(VERSION)" -date "$(CHANGELOG_DATE)" -repo "$(CHANGELOG_REPO)"

.PHONY: release-snapshot
# scripts/run-goreleaser.sh fetches goreleaser and syft into a new directory
# for each run, so neither target runs a binary from .tools/bin, from PATH or
# from an earlier run.
release-snapshot: ## Build unsigned, endpoint-neutral release artifacts
	GORELEASER_VERSION=$(GORELEASER_VERSION) ./scripts/run-goreleaser.sh release --snapshot --clean --skip=sign
	./scripts/place-manifest.sh dist

.PHONY: release
release: ## Build endpoint-neutral artifacts from a release tag
	GORELEASER_VERSION=$(GORELEASER_VERSION) ./scripts/run-goreleaser.sh release --clean
	./scripts/place-manifest.sh dist

.PHONY: release-validate
release-validate: release-snapshot ## Build a snapshot and verify its provider archive layout
	REQUIRE_SIGNATURE=false ./scripts/validate-release-artifacts.sh dist

.PHONY: release-config-test
release-config-test: ## Verify the README dev_overrides selector; go test checks the release workflows
	./scripts/tests/test-release-dispatch-config.sh

.PHONY: install-syft-test
install-syft-test: ## Refuse a syft download whose checksum isn't the pinned one
	./scripts/tests/test-install-syft.sh

.PHONY: workflow-actions-test
workflow-actions-test: ## Refuse an unpinned action, a kept token or keyring, or a vulncheck that skips the release Go
	./scripts/tests/test-workflow-actions.sh

.PHONY: major-release-gate-test
major-release-gate-test: ## Verify the major version gate refuses and explains
	./scripts/tests/test-major-release-gate.sh

.PHONY: release-tag-validation-test
release-tag-validation-test: ## Run the release dispatch's tag checks against a scratch repository
	./scripts/tests/test-release-tag-validation.sh

.PHONY: signing-key-check
signing-key-check: ## Warn when the committed signing subkey is due to rotate, and fail when it's near expiry
	./scripts/check-signing-key.sh public docs/signing-key.asc

.PHONY: signing-key-check-test
signing-key-check-test: ## Run the signing key check against throwaway keys
	./scripts/tests/test-signing-key-check.sh

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
	rm -rf dist .tools
