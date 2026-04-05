PROJECT_ROOT := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))

EMBED_MODEL ?= nomic-embed-text:latest
EMBED_PROVIDER ?= ollama
EMBED_MODEL_CONTEXT_LENGTH ?= 2048
TASK_MODEL ?= phi3:3.8b
TASK_MODEL_CONTEXT_LENGTH ?= 2048
TASK_PROVIDER ?= ollama
CHAT_MODEL ?= phi3:3.8b
CHAT_PROVIDER ?= ollama
CHAT_MODEL_CONTEXT_LENGTH ?= 2048
TENANCY ?= 54882f1d-3788-44f9-aed6-19a793c4568f
# Ollama HTTP API (host:port). Same default as apitests/conftest.py — override per machine (e.g. Docker bridge).
OLLAMA_HOST ?= 127.0.0.1:11434

export EMBED_MODEL EMBED_PROVIDER EMBED_MODEL_CONTEXT_LENGTH
export TASK_MODEL TASK_MODEL_CONTEXT_LENGTH TASK_PROVIDER
export CHAT_MODEL CHAT_MODEL_CONTEXT_LENGTH CHAT_PROVIDER
export TENANCY
export OLLAMA_HOST

.PHONY: build-contenox verify-cli-help run-contenox \
        run-beam-bg stop-beam-bg \
        start-ollama-pull start-ollama ollama-status \
        test test-unit test-system test-contenoxcli \
        test-api test-api-full test-api-init install-api-test-deps wait-for-server \
        docs-gen docs-client-smoke docs-markdown docs-html \
        website-dev site-build website-install website-clean \
        set-version bump-major bump-minor bump-patch \
        commit-docs release enterprise-clean \
        install-air air-beam

# --------------------------------------------------------------------
# Contenox CLI
# --------------------------------------------------------------------
# Version comes from apiframework/version.txt (embedded via apiframework); optional
# link-time override: -ldflags "-X github.com/contenox/contenox/internal/contenoxcli.Version=…"
build-contenox:
	go build -o $(PROJECT_ROOT)/bin/contenox $(PROJECT_ROOT)/cmd/contenox

# Smoke-test Cobra help for main commands (sync with docs/reference/contenox-cli.md).
verify-cli-help: build-contenox
	@chmod +x $(PROJECT_ROOT)/scripts/verify_cli_help.sh
	@CONTENOX_BIN=$(PROJECT_ROOT)/bin/contenox $(PROJECT_ROOT)/scripts/verify_cli_help.sh

run-contenox: build-contenox
	$(PROJECT_ROOT)/bin/contenox $(ARGS)

# Local dev shorthand
DEV_BIN := $(HOME)/.local/bin/contenox

dev: build-contenox dev-link
	@echo "→ dev binary: $(PROJECT_ROOT)/bin/contenox"
	@echo "→ symlink:    $(DEV_BIN)"
	@echo "   Make sure ~/.local/bin appears before /usr/local/bin in PATH."

dev-link: build-contenox
	@mkdir -p $(dir $(DEV_BIN))
	@ln -sf $(PROJECT_ROOT)/bin/contenox $(DEV_BIN)
	@echo "Linked $(DEV_BIN) → $(PROJECT_ROOT)/bin/contenox"

dev-unlink:
	@rm -f $(DEV_BIN)
	@echo "Removed $(DEV_BIN)"

# --------------------------------------------------------------------
# MCP Test Server
# --------------------------------------------------------------------
build-mcp-testserver:
	go build -o $(PROJECT_ROOT)/bin/mcp-testserver $(PROJECT_ROOT)/cmd/mcp-testserver

docker-build-mcp-testserver:
	docker build -t contenox/mcp-testserver:local \
		-f $(PROJECT_ROOT)/cmd/mcp-testserver/Dockerfile \
		$(PROJECT_ROOT)

run-mcp-testserver: build-mcp-testserver
	$(PROJECT_ROOT)/bin/mcp-testserver

docker-run-mcp-testserver: docker-build-mcp-testserver
	docker run --rm -p 8090:8090 contenox/mcp-testserver:local

run-mcp-testserver-bg: build-mcp-testserver
	@pkill -f bin/mcp-testserver 2>/dev/null || true
	@sleep 1
	@$(PROJECT_ROOT)/bin/mcp-testserver &
	@sleep 1
	@curl -sf http://localhost:8090/health && echo " mcp-testserver ready" || (echo "ERROR: mcp-testserver failed to start"; exit 1)

docker-run-mcp-testserver-bg: docker-build-mcp-testserver
	@docker rm -f mcp-testserver-bg 2>/dev/null || true
	@docker run --rm -d --name mcp-testserver-bg -p 8090:8090 contenox/mcp-testserver:local
	@sleep 2
	@curl -sf http://localhost:8090/health && echo " mcp-testserver (docker) ready" || (echo "ERROR: mcp-testserver container failed"; docker logs mcp-testserver-bg; exit 1)

test-mcp-session: build-contenox docker-run-mcp-testserver-bg
	@echo "=== MCP session persistence test ==="
	$(PROJECT_ROOT)/bin/contenox run \
		--chain $(PROJECT_ROOT)/.contenox/chain-mcp-session-test.json \
		"call session_set key=player value=Alice, then call session_dump. Confirm if the session_tokens match exactly."
	@echo "=== Expected: session_token identical across all tool calls ==="

# --------------------------------------------------------------------
# Ollama
# --------------------------------------------------------------------
ollama-status:
	@echo "Checking Ollama status at $(OLLAMA_HOST)..."
	@curl -s -f http://$(OLLAMA_HOST)/api/tags > /dev/null || (echo "Error: Ollama server not responding at $(OLLAMA_HOST). Start it with 'ollama serve' or check OLLAMA_HOST." && exit 1)
	@echo "Ollama is reachable."

start-ollama-pull: ollama-status
	OLLAMA_HOST=$(OLLAMA_HOST) ollama pull $(TASK_MODEL)

start-ollama: start-ollama-pull
	@echo "Model $(TASK_MODEL) ready at $(OLLAMA_HOST)."

# --------------------------------------------------------------------
# Tests
# --------------------------------------------------------------------
test:
	GOMAXPROCS=4 go test -C $(PROJECT_ROOT) ./...

test-unit:
	GOMAXPROCS=4 go test -C $(PROJECT_ROOT) -run '^TestUnit_' ./...

test-system:
	GOMAXPROCS=4 go test -C $(PROJECT_ROOT) -run '^TestSystem_' ./...

test-contenoxcli:
	GOMAXPROCS=4 go test -C $(PROJECT_ROOT) -v ./internal/contenoxcli/...

# --------------------------------------------------------------------
# API tests
# --------------------------------------------------------------------
#   make install-api-test-deps  — Python venv + pip (once)
#   make test-api     — build beam, temp data dir, pytest, teardown (good default)
#   make test-api-full         — run-beam-bg, then test-api (server already up), stop-beam-bg
#   make test-api               — pytest only; needs :8081 healthy (see wait-for-server)
APITEST_VENV := $(PROJECT_ROOT)/apitests/.venv
APITEST_ACTIVATE := $(APITEST_VENV)/bin/activate

# Create apitests/.venv and install Python packages (pytest, requests, testcontainers, …).
# Requires: python3 and the venv module (e.g. apt install python3-venv on Debian/Ubuntu).
# Health checks use wget; install wget or curl if missing. For tests: also `make build-contenox`.
test-api-init:
	test -d $(APITEST_VENV) || python3 -m venv $(APITEST_VENV)
	. $(APITEST_ACTIVATE) && pip install -r $(PROJECT_ROOT)/apitests/requirements.txt

install-api-test-deps: test-api-init
	@echo "API test Python dependencies installed under $(APITEST_VENV)"

wait-for-server:
	@echo "Waiting for server..."
	@until wget --spider -q http://localhost:8081/health; do \
		echo "Still waiting..."; sleep 2; \
	done

# Start `contenox beam` in the background (default :8081). Use before test-api / apitests.
run-beam-bg: build-contenox
	@pkill -f "[/]bin/contenox beam" 2>/dev/null || true
	@sleep 1
	@cd $(PROJECT_ROOT) && $(PROJECT_ROOT)/bin/contenox beam &
	@sleep 2
	@curl -sf http://localhost:8081/health >/dev/null && echo "contenox beam ready on :8081" || (echo "ERROR: contenox beam failed to start"; exit 1)

stop-beam-bg:
	@pkill -f "[/]bin/contenox beam" 2>/dev/null || true

test-api-full: run-beam-bg test-api stop-beam-bg

# Self-contained: temp data dir, start beam, wait for /health, pytest, stop beam.
# Use this when nothing is serving :8081 yet (avoids the wait-for-server deadlock).
# pkill runs on its own recipe line so -f cannot match the long sh -c that starts beam.
test-api: build-contenox test-api-init
	@pkill -f "[/]bin/contenox beam" 2>/dev/null || true
	@sleep 1
	@TMPDIR=$$(mktemp -d) && \
	echo "Using temp data dir: $$TMPDIR" && \
	mkdir -p "$$TMPDIR/.contenox" && \
	$(PROJECT_ROOT)/bin/contenox beam --data-dir "$$TMPDIR/.contenox" & \
	BEAM_PID=$$! && \
	echo "Started beam (pid $$BEAM_PID)" && \
	until wget --spider -q http://localhost:8081/health 2>/dev/null; do sleep 1; done && \
	echo "Beam ready" && \
	( . $(APITEST_ACTIVATE) && OLLAMA_HOST=$(OLLAMA_HOST) pytest $(PROJECT_ROOT)/apitests/$(TEST_FILE) ; ) ; \
	TEST_EXIT=$$? && \
	kill $$BEAM_PID 2>/dev/null; wait $$BEAM_PID 2>/dev/null; \
	rm -rf "$$TMPDIR" && \
	echo "Cleaned up $$TMPDIR" && \
	exit $$TEST_EXIT

# --------------------------------------------------------------------
# Documentation & Versioning
# --------------------------------------------------------------------
docs-gen:
	go run $(PROJECT_ROOT)/tools/openapi-gen \
		--project="$(PROJECT_ROOT)" \
		--output="$(PROJECT_ROOT)/docs"

docs-client-smoke: docs-gen
	@chmod +x $(PROJECT_ROOT)/scripts/verify_openapi_client.sh
	@OPENAPI_SPEC=$(PROJECT_ROOT)/docs/openapi.json $(PROJECT_ROOT)/scripts/verify_openapi_client.sh

docs-markdown: docs-gen
	docker run --rm \
		-v $(PROJECT_ROOT)/docs:/local \
		node:24-alpine sh -c "\
			npm install -g widdershins@4 && \
			widdershins /local/openapi.json -o /local/api-reference.md \
			--summary --resolve --verbose \
		"

docs-html: docs-gen
	mkdir -p $(PROJECT_ROOT)/docs
	cp $(PROJECT_ROOT)/scripts/openapi-rapidoc.html $(PROJECT_ROOT)/docs/openapi.html

set-version:
	go run $(PROJECT_ROOT)/tools/version/main.go set

bump-patch:
	go run $(PROJECT_ROOT)/tools/version/main.go bump patch

bump-minor:
	go run $(PROJECT_ROOT)/tools/version/main.go bump minor

bump-major:
	go run $(PROJECT_ROOT)/tools/version/main.go bump major

site-build: website-install
	cd $(PROJECT_ROOT)/enterprise/site && npm run build

website-dev: website-install
	cd $(PROJECT_ROOT)/enterprise/site && npm run dev

website-install:
	cd $(PROJECT_ROOT)/enterprise/site && npm install

website-clean:
	rm -rf $(PROJECT_ROOT)/enterprise/site/.next

commit-docs: docs-markdown docs-html
	git add $(PROJECT_ROOT)/docs
	-git commit -S -m "chore: update api docs"

release: docs-markdown docs-html
	git add $(PROJECT_ROOT)/docs
	-git commit -S -m "chore: update api docs for release"
	@echo "Release assets prepared. Push a version tag to trigger the release workflow (binary artifacts on GitHub Releases)."

# =============================================================================
# Air — live reload for Go (https://github.com/air-verse/air)
# Terminal 1: make air-beam   |   Terminal 2: make dev-server-proxy (Vite + /api proxy)
# =============================================================================

# Prefer `air` on PATH; else $(go env GOPATH)/bin/air (where go install puts it).
AIR ?= $(shell command -v air 2>/dev/null || echo "$(shell go env GOPATH)/bin/air")

install-air:
	go install github.com/air-verse/air@latest
	@echo "Air: $(AIR)"

air-beam:
	@test -x "$(AIR)" || { echo "air not found at $(AIR). Run: make install-air"; exit 1; }
	cd $(PROJECT_ROOT) && "$(AIR)" -c .air.toml

# =============================================================================
# npm‑based commands (workspaces: beam and ui)
# =============================================================================

# Clean everything (root + packages)
clean:
	rm -rf node_modules packages/beam/node_modules packages/ui/node_modules package-lock.json
	rm -rf packages/beam/dist packages/ui/dist
	rm -rf .vite

# Full fresh install (npm workspaces from root)
install:
	npm install

# Build the UI package (Beam build runs scripts/beam_embed_stamp.sh so Go //go:embed
# picks up UI changes on the next build-contenox).
ui-build:
	npm run build --workspace=@contenox/ui
	npm run build --workspace=@contenox/beam

# Run development server (beam)
dev-server:
	npm run dev --workspace=@contenox/beam

# Vite + /api proxy to contenox (set VITE_DEV_API_PROXY=1). Run `contenox beam` first.
dev-server-proxy:
	npm run dev:proxy --workspace=@contenox/beam

# Build the main beam package
build-beam:
	npm run build --workspace=@contenox/beam

# Lint beam
lint-beam:
	npm run lint --workspace=@contenox/beam

# Format code (beam)
format-beam:
	npm run format --workspace=@contenox/beam

# Clean + reinstall + run dev (most useful command)
fresh-dev: clean install dev-server

# Legacy targets
yarn-wipe:
	@echo "This project no longer uses Yarn."
	@echo "Use 'make clean' and 'make install' instead."

ui-install:
	@echo "Use 'make install' instead (npm workspaces)"

ui-package:
	@echo "Use 'make ui-build' instead"
