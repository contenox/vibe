# Contenox — namespaces: build-*  test-*  docs-*  dev-*  deps-*
# Default: make help

PROJECT_ROOT := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))
.DEFAULT_GOAL := help

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
OLLAMA_HOST ?= 127.0.0.1:11434

export EMBED_MODEL EMBED_PROVIDER EMBED_MODEL_CONTEXT_LENGTH
export TASK_MODEL TASK_MODEL_CONTEXT_LENGTH TASK_PROVIDER
export CHAT_MODEL CHAT_MODEL_CONTEXT_LENGTH CHAT_PROVIDER
export TENANCY
export OLLAMA_HOST

AIR ?= $(shell command -v air 2>/dev/null || echo "$(shell go env GOPATH)/bin/air")
APITEST_VENV := $(PROJECT_ROOT)/apitests/.venv
APITEST_ACTIVATE := $(APITEST_VENV)/bin/activate
DEV_CLI_BIN := $(HOME)/.local/bin/contenox

.PHONY: help \
	build-cli build-web ci-prepare-embeds \
	clean \
	deps-go-watch deps-npm \
	dev-cli dev-cli-link dev-cli-unlink \
	dev-go-watch dev-web dev-web-fresh dev-web-proxy \
	docs-gen docs-html docs-markdown \
	format-web lint-web \
	test test-unit test-system test-cli-verbose test-cli-help \
	test-http-api test-http-api-venv test-openapi-client-codegen \
	wait-http-ready

# -----------------------------------------------------------------------------
help:
	@echo "build-*    build-cli build-web  |  ci-prepare-embeds (CI: Beam dist + OpenAPI stub)"
	@echo "test-*     test test-unit test-system test-cli-verbose test-cli-help"
	@echo "           test-http-api test-http-api-venv test-openapi-client-codegen"
	@echo "docs-*     docs-gen docs-html docs-markdown"
	@echo "dev-*      dev-cli dev-cli-link dev-cli-unlink dev-go-watch dev-web dev-web-proxy dev-web-fresh"
	@echo "deps-*     deps-npm deps-go-watch"
	@echo "lint-web format-web  |  wait-http-ready"
	@echo "Version (maintainers): make -f Makefile.version help"
	@echo "clean"
	@echo "See CONTRIBUTING.md for the two-terminal web + API workflow."

# —— build ————————————————————————————————————————————————————————————————
# CI: generate gitignored embed inputs (Beam dist + OpenAPI stub) before docs-gen / go build.
ci-prepare-embeds:
	bash $(PROJECT_ROOT)/scripts/ci_prepare_embeds.sh

build-cli:
	@test -f $(PROJECT_ROOT)/internal/openapidocs/openapi.json || $(MAKE) docs-gen
	go build -o $(PROJECT_ROOT)/bin/contenox $(PROJECT_ROOT)/cmd/contenox

build-web:
	npm run build --workspace=@contenox/ui
	npm run build --workspace=@contenox/beam

# —— test ————————————————————————————————————————————————————————————————
test:
	GOMAXPROCS=4 go test -C $(PROJECT_ROOT) ./...

test-unit:
	@test -f $(PROJECT_ROOT)/internal/openapidocs/openapi.json || $(MAKE) docs-gen
	GOMAXPROCS=4 go test -C $(PROJECT_ROOT) -short -timeout 15m -run '^TestUnit_' ./...

test-system:
	GOMAXPROCS=4 go test -C $(PROJECT_ROOT) -run '^TestSystem_' ./...

test-cli-verbose:
	GOMAXPROCS=4 go test -C $(PROJECT_ROOT) -v ./internal/contenoxcli/...

test-cli-help: build-cli
	@chmod +x $(PROJECT_ROOT)/scripts/verify_cli_help.sh
	@CONTENOX_BIN=$(PROJECT_ROOT)/bin/contenox $(PROJECT_ROOT)/scripts/verify_cli_help.sh

test-http-api-venv:
	test -d $(APITEST_VENV) || python3 -m venv $(APITEST_VENV)
	. $(APITEST_ACTIVATE) && pip install -r $(PROJECT_ROOT)/apitests/requirements.txt

test-http-api: build-cli test-http-api-venv
	@pkill -f "[/]bin/contenox beam" 2>/dev/null || true
	@sleep 1
	@TMPDIR=$$(mktemp -d) && \
	mkdir -p "$$TMPDIR/.contenox" && \
	$(PROJECT_ROOT)/bin/contenox --data-dir "$$TMPDIR/.contenox" config set default-model "$(TASK_MODEL)" && \
	$(PROJECT_ROOT)/bin/contenox --data-dir "$$TMPDIR/.contenox" config set default-provider "$(TASK_PROVIDER)" && \
	$(PROJECT_ROOT)/bin/contenox beam --data-dir "$$TMPDIR/.contenox" & \
	BEAM_PID=$$! && \
	until curl -sf -o /dev/null http://localhost:8081/api/health 2>/dev/null; do sleep 1; done && \
	( . $(APITEST_ACTIVATE) && OLLAMA_HOST=$(OLLAMA_HOST) pytest $(PROJECT_ROOT)/apitests/$(TEST_FILE) ; ) ; \
	TEST_EXIT=$$? && \
	kill $$BEAM_PID 2>/dev/null; wait $$BEAM_PID 2>/dev/null; \
	rm -rf "$$TMPDIR" && \
	exit $$TEST_EXIT

test-openapi-client-codegen: docs-gen
	@chmod +x $(PROJECT_ROOT)/scripts/verify_openapi_client.sh
	@OPENAPI_SPEC=$(PROJECT_ROOT)/docs/openapi.json $(PROJECT_ROOT)/scripts/verify_openapi_client.sh

# —— docs ————————————————————————————————————————————————————————————————
docs-gen:
	go run $(PROJECT_ROOT)/tools/openapi-gen \
		--project="$(PROJECT_ROOT)" \
		--output="$(PROJECT_ROOT)/docs"
	cp $(PROJECT_ROOT)/docs/openapi.json $(PROJECT_ROOT)/internal/openapidocs/openapi.json

docs-markdown: docs-gen
	@if command -v docker >/dev/null 2>&1; then \
		docker run --rm \
			-v $(PROJECT_ROOT)/docs:/local \
			node:24-alpine sh -c "\
				npm install -g widdershins@4 && \
				widdershins /local/openapi.json -o /local/api-reference.md \
				--summary --resolve --verbose \
			"; \
	else \
		cd $(PROJECT_ROOT)/docs && npx -y widdershins@4 openapi.json -o api-reference.md --summary --resolve --verbose; \
	fi

docs-html: docs-gen
	mkdir -p $(PROJECT_ROOT)/docs
	cp $(PROJECT_ROOT)/scripts/openapi-rapidoc.html $(PROJECT_ROOT)/docs/openapi.html

# —— dev —————————————————————————————————————————————————————————————————
dev-cli: build-cli dev-cli-link

dev-cli-link: build-cli
	@mkdir -p $(dir $(DEV_CLI_BIN))
	@ln -sf $(PROJECT_ROOT)/bin/contenox $(DEV_CLI_BIN)

dev-cli-unlink:
	@rm -f $(DEV_CLI_BIN)

dev-go-watch:
	@test -x "$(AIR)" || { echo "run: make deps-go-watch"; exit 1; }
	cd $(PROJECT_ROOT) && "$(AIR)" -c .air.toml

dev-web:
	npm run dev --workspace=@contenox/beam

dev-web-proxy:
	npm run dev:proxy --workspace=@contenox/beam

dev-web-fresh: clean deps-npm dev-web

wait-http-ready:
	@until curl -sf -o /dev/null http://localhost:8081/api/health; do sleep 2; done

lint-web:
	npm run lint --workspace=@contenox/beam

format-web:
	npm run format --workspace=@contenox/beam

# —— deps ———————————————————————————————————————————————————————————————
deps-npm:
	npm install

deps-go-watch:
	go install github.com/air-verse/air@latest

# —— clean ———————————————————————————————————————————————————————————————
clean:
	rm -rf node_modules packages/beam/node_modules packages/ui/node_modules package-lock.json
	rm -rf packages/beam/dist packages/ui/dist
	rm -rf .vite
