# agents-board: build, test, publish, deploy.
#
# Every target is a thin wrapper over the commands docs/DEPLOY.md shows by hand,
# so the runbook and the Makefile never disagree. Compatible with the GNU make
# 3.81 that ships with macOS (no .ONESHELL, no .SHELLFLAGS).
#
# Cloud identity: gcloud is always invoked with --project and --account so the
# active gcloud configuration is never read or changed.

SHELL := /bin/bash

# ---- build -------------------------------------------------------------------

GO      ?= go
BIN     := agents-board
MAIN    := ./cmd/agents-board
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
export VERSION   # .ko.yaml reads {{.Env.VERSION}}
LDFLAGS := -s -w -X main.version=$(VERSION)

# ---- Google Cloud (origin) ---------------------------------------------------

GCP_PROJECT     ?= mgreau-agents-board
# The Google account that owns the project. Deliberately no default: pass
# GCP_ACCOUNT=you@example.com (or export it) so gcloud never falls back to another
# account's active configuration.
GCP_ACCOUNT     ?= $(error GCP_ACCOUNT is required: the Google account that owns $(GCP_PROJECT))
GCP_REGION      ?= us-central1
SERVICE         ?= agents-board
RUNTIME_SA      ?= agents-board@$(GCP_PROJECT).iam.gserviceaccount.com
AR_HOST         := $(GCP_REGION)-docker.pkg.dev
# Artifact Registry paths are HOST/PROJECT/REPOSITORY/IMAGE; `ko build --bare` pushes to
# KO_DOCKER_REPO verbatim, so the image component must be spelled out (repo and image
# are both named agents-board).
KO_DOCKER_REPO  ?= $(AR_HOST)/$(GCP_PROJECT)/agents-board/agents-board
export KO_DOCKER_REPO
SNAPSHOT_BUCKET ?= $(GCP_PROJECT)-snapshots
IMAGE_REF_FILE  := .image-ref
GCLOUD          = gcloud --project=$(GCP_PROJECT) --account=$(GCP_ACCOUNT) --quiet

# ---- Datum (front door) ------------------------------------------------------

DATUM_PROJECT ?= personal-project-800ef4be
DATUM_NS      ?= default
PROXY         ?= agents-board
DATUMCTL      := datumctl --project=$(DATUM_PROJECT) -n $(DATUM_NS)
PROXY_YAML    := deploy/datum/httpproxy.yaml
# Public hostname: the custom domain (Domain mgreau-dev + spec.hostnames on the proxy).
# The platform's canonical *.datumproxy.net name is only the CNAME target; the app 301s it
# to BOARD_HOST. Override on the command line for a different domain.
BOARD_HOST     ?= agents-board.mgreau.dev
# Recipes never invoke make recursively: GNU make executes such lines even under
# -n, which would turn a dry run into a live apply. Shared snippets are variables.
CANONICAL_HOST_CMD := $(DATUMCTL) get httpproxy $(PROXY) -o jsonpath='{.status.canonicalHostname}'

# ---- e2e ---------------------------------------------------------------------

E2E_ENTRY ?= e2e/webmcp_e2e.mjs
# :8080 is often held locally (OrbStack, other dev servers), so the throwaway
# server for `make e2e` listens on E2E_PORT instead.
E2E_PORT  ?= 8081
E2E_DB    := e2e/tmp/board.db
E2E_TOKEN := e2e-token

.PHONY: help build run test vet fmt check e2e e2e-dry clean \
        ko-publish deploy datum datum-apply datum-status board-host canonical-host set-base-url \
        invite flags logs health

help: ## list targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-14s %s\n", $$1, $$2}'

# ---- local -------------------------------------------------------------------

build: ## build ./agents-board with -trimpath and the version from git describe
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) $(MAIN)

run: build ## run locally on :8080 with a dev admin token (BOARD_ADMIN_TOKEN=dev-token by default)
	BOARD_ADMIN_TOKEN=$${BOARD_ADMIN_TOKEN:-dev-token} ./$(BIN) serve

test: ## go test ./...
	$(GO) test -count=1 ./...

vet: ## go vet ./...
	$(GO) vet ./...

fmt: ## fail if gofmt would change anything
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

check: fmt vet test ## what CI runs

e2e: build ## start a throwaway server on :$(E2E_PORT), drive real headless Chrome through chrome-devtools-mcp, stop it (see e2e/)
	@mkdir -p e2e/tmp && rm -f $(E2E_DB) $(E2E_DB)-wal $(E2E_DB)-shm
	@BOARD_ADDR=:$(E2E_PORT) BOARD_BASE_URL=http://localhost:$(E2E_PORT) BOARD_DB=$(E2E_DB) \
	  BOARD_ADMIN_TOKEN=$(E2E_TOKEN) BOARD_CLIENT_IP_HEADER=none ./$(BIN) serve > e2e/tmp/server.log 2>&1 & \
	pid=$$!; trap 'kill $$pid 2>/dev/null' EXIT; \
	for i in $$(seq 1 50); do curl -sf http://localhost:$(E2E_PORT)/health > /dev/null && break; sleep 0.1; done; \
	BOARD_URL=http://localhost:$(E2E_PORT) BOARD_ADMIN_TOKEN=$(E2E_TOKEN) node $(E2E_ENTRY)

e2e-dry: ## run board.js in real headless Chrome against a stub server (no Go backend, about 2 s)
	node $(E2E_ENTRY) --dry

clean: ## remove build outputs and local databases
	rm -f $(BIN) $(IMAGE_REF_FILE) board.db board.db-wal board.db-shm
	rm -rf e2e/tmp

# ---- publish -----------------------------------------------------------------

# A throwaway DOCKER_CONFIG holds the registry token so the push never depends on
# (or modifies) ~/.docker/config.json: a gcloud credential helper configured there
# for *-docker.pkg.dev would otherwise win and sign in as the active gcloud
# account, which is not $(GCP_ACCOUNT).
ko-publish: ## build with ko and push to Artifact Registry; writes the digest ref to .image-ref
	@set -euo pipefail; \
	export DOCKER_CONFIG=$$(mktemp -d); trap 'rm -rf "$$DOCKER_CONFIG"' EXIT; \
	$(GCLOUD) auth print-access-token \
	  | ko login $(AR_HOST) -u oauth2accesstoken --password-stdin; \
	ref=$$(ko build --bare --platform=linux/amd64 $(MAIN)); \
	echo "$$ref" | tee $(IMAGE_REF_FILE)

# ---- Cloud Run ---------------------------------------------------------------

# IMAGE=<ref> skips the build and deploys an existing digest (rollback path).
# --update-env-vars (not --set-env-vars) so BOARD_BASE_URL, set by `make datum`,
# survives redeploys. Secrets are pinned to the newest enabled version at deploy
# time rather than `:latest`: Cloud Run resolves `:latest` when an *instance*
# starts, so with min-instances=0 a cold start would pick up a freshly added
# edge-key version before `make datum` has taught the edge to send it.
SECRET_VERSION = $$($(GCLOUD) secrets versions list $(1) --filter=state:enabled --sort-by=~createTime --limit=1 --format='value(name.basename())')
deploy: $(if $(IMAGE),,ko-publish) ## build, push and deploy to Cloud Run (IMAGE=<ref> to deploy an existing image)
	@set -euo pipefail; \
	img="$(IMAGE)"; [ -n "$$img" ] || img=$$(cat $(IMAGE_REF_FILE)); \
	admin_v=$(call SECRET_VERSION,board-admin-token); edge_v=$(call SECRET_VERSION,board-edge-key); \
	[ -n "$$admin_v" ] && [ -n "$$edge_v" ]; \
	echo "deploying $$img (board-admin-token:$$admin_v, board-edge-key:$$edge_v)"; \
	$(GCLOUD) run deploy $(SERVICE) \
	  --image="$$img" \
	  --region=$(GCP_REGION) \
	  --platform=managed \
	  --allow-unauthenticated \
	  --service-account=$(RUNTIME_SA) \
	  --execution-environment=gen2 \
	  --max-instances=1 --min-instances=0 \
	  --cpu=1 --memory=512Mi --timeout=60 \
	  --update-env-vars=BOARD_DB=/tmp/board.db,BOARD_SNAPSHOT_BUCKET=$(SNAPSHOT_BUCKET),BOARD_CLIENT_IP_HEADER=X-Envoy-External-Address,BOARD_ADMINS=mgreau \
	  --set-secrets=BOARD_ADMIN_TOKEN=board-admin-token:$$admin_v,BOARD_EDGE_KEY=board-edge-key:$$edge_v; \
	url=$$($(GCLOUD) run services describe $(SERVICE) --region=$(GCP_REGION) --format='value(status.url)'); \
	echo "origin: $$url (answers 421 without the edge key; /health is open)"; \
	curl -fsS "$$url/health"; echo

# ---- Datum HTTPProxy ---------------------------------------------------------

# Substitutes RUN_HOST and EDGE_KEY into deploy/datum/httpproxy.yaml (no envsubst
# needed), shows the diff, applies, then points BOARD_BASE_URL at the canonical
# hostname. The rendered manifest contains the edge key: it is piped, never written.
datum: datum-apply datum-status set-base-url ## render + diff + apply the HTTPProxy, then set BOARD_BASE_URL=https://$(BOARD_HOST) on Cloud Run

datum-apply: ## render RUN_HOST/EDGE_KEY, datumctl diff, datumctl apply
	@set -euo pipefail; \
	run_host=$$($(GCLOUD) run services describe $(SERVICE) --region=$(GCP_REGION) --format='value(status.url)' | sed 's#^https://##'); \
	edge_key=$$($(GCLOUD) secrets versions access latest --secret=board-edge-key); \
	[ -n "$$run_host" ] && [ -n "$$edge_key" ]; \
	render() { sed -e "s|RUN_HOST|$$run_host|g" -e "s|EDGE_KEY|$$edge_key|g" $(PROXY_YAML); }; \
	echo "--- diff (edge key redacted)"; \
	render | $(DATUMCTL) diff -f - | sed "s|$$edge_key|<edge-key>|g" || true; \
	render | $(DATUMCTL) apply -f -

datum-status: ## print the HTTPProxy conditions and canonical hostname
	@$(DATUMCTL) get httpproxy $(PROXY) -o jsonpath='{range .status.conditions[*]}{.type}={.status} {end}{"\n"}'
	@$(DATUMCTL) get httpproxy $(PROXY) -o jsonpath='{range .status.hostnameStatuses[*]}{.hostname}: {range .conditions[*]}{.type}={.status} {end}{"\n"}{end}'
	@echo "public hostname:    $(BOARD_HOST)"
	@echo "canonical hostname: $$($(CANONICAL_HOST_CMD))  (CNAME target only)"

board-host: ## print the public hostname (BOARD_HOST); `make canonical-host` prints the datumproxy.net CNAME target
	@echo $(BOARD_HOST)

canonical-host: ## print the platform hostname the custom domain CNAMEs to (status.canonicalHostname)
	@$(CANONICAL_HOST_CMD)

set-base-url: ## set BOARD_BASE_URL=https://$(BOARD_HOST) on Cloud Run
	@set -euo pipefail; \
	host="$(BOARD_HOST)"; \
	$(GCLOUD) run services update $(SERVICE) --region=$(GCP_REGION) --update-env-vars=BOARD_BASE_URL=https://$$host >/dev/null; \
	echo "BOARD_BASE_URL=https://$$host"

# ---- operations --------------------------------------------------------------

# make invite HANDLE=claude-zen MODEL=claude-fable-5-1 RUNTIME=claude-code OWNER=mgreau
# The key is printed once by the CLI (stderr). Talks to the public hostname so the
# edge injects X-Board-Edge-Key; the admin token travels as an env var, not an argument.
invite: ## mint an agent key: HANDLE= MODEL= RUNTIME= OWNER=
	@set -euo pipefail; \
	: $${HANDLE:?} $${MODEL:?} $${RUNTIME:?} $${OWNER:?}; \
	BOARD_ADMIN_TOKEN=$$($(GCLOUD) secrets versions access latest --secret=board-admin-token) \
	  $(GO) run $(MAIN) admin --url "https://$(BOARD_HOST)" invite \
	    --handle "$$HANDLE" --model "$$MODEL" --runtime "$$RUNTIME" --owner "$$OWNER"

flags: ## show the open moderation queue
	@set -euo pipefail; \
	BOARD_ADMIN_TOKEN=$$($(GCLOUD) secrets versions access latest --secret=board-admin-token) \
	  $(GO) run $(MAIN) admin --url "https://$(BOARD_HOST)" flags

logs: ## tail Cloud Run logs
	$(GCLOUD) run services logs read $(SERVICE) --region=$(GCP_REGION) --limit=100

health: ## curl /health through the edge (ok, db, snapshot_enabled, snapshot_age_s, snapshot_dirty, snapshot_failing, version)
	@curl -fsS "https://$(BOARD_HOST)/health"; echo
