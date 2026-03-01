PROJECT_ROOT := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))
VERSION_FILE := $(PROJECT_ROOT)/apiframework/version.txt

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
OLLAMA_HOST ?= 172.17.0.1:11434

export EMBED_MODEL EMBED_PROVIDER EMBED_MODEL_CONTEXT_LENGTH
export TASK_MODEL TASK_MODEL_CONTEXT_LENGTH TASK_PROVIDER
export CHAT_MODEL CHAT_MODEL_CONTEXT_LENGTH CHAT_PROVIDER
export TENANCY
export OLLAMA_HOST

# Allow user override of COMPOSE_CMD
COMPOSE_CMD ?= docker compose -f compose.yaml -f compose.local.yaml

.PHONY: run-runtime-api build-runtime-api \
        docker-build-runtime docker-up-runtime docker-run-runtime \
        docker-down-runtime docker-clear-runtime docker-logs-runtime \
        build-vibe run-vibe \
        start-ollama-pull start-ollama ollama-status \
        test test-unit test-system test-vibecli \
        test-api test-api-full test-api-init wait-for-server \
        docs-gen docs-markdown docs-html \
        set-version bump-major bump-minor bump-patch \
        commit-docs release


# --------------------------------------------------------------------
# Docker Runtime lifecycle
# --------------------------------------------------------------------
docker-build-runtime:
	$(COMPOSE_CMD) build --build-arg TENANCY=$(TENANCY)

docker-up-runtime:
	$(COMPOSE_CMD) up -d

docker-run-runtime: docker-down-runtime docker-build-runtime docker-up-runtime

docker-down-runtime:
	$(COMPOSE_CMD) down

docker-clear-runtime:
	$(COMPOSE_CMD) down --volumes --remove-orphans

docker-logs-runtime:
	$(COMPOSE_CMD) logs -f runtime-api

# --------------------------------------------------------------------
# Vibe CLI --------------------------------------------------------------------
build-vibe:
	go build -o $(PROJECT_ROOT)/bin/vibe $(PROJECT_ROOT)/cmd/vibe

# Run the Vibe binary (builds if needed). Example: make run-vibe ARGS="-input 'hello'"
run-vibe: build-vibe
	$(PROJECT_ROOT)/bin/vibe $(ARGS)

# --------------------------------------------------------------------
# Local Runtime API: HTTP server
# --------------------------------------------------------------------
build-runtime-api:
	go build -o $(PROJECT_ROOT)/bin/runtime-api $(PROJECT_ROOT)/cmd/runtime

run-runtime-api: build-runtime-api
	@echo "Run with env: DATABASE_URL, NATS_URL, TOKENIZER_SERVICE_URL, PORT, etc. Example:"
	@echo "  DATABASE_URL=postgres://... NATS_URL=nats://... TOKENIZER_SERVICE_URL=... ./bin/runtime-api"
	$(PROJECT_ROOT)/bin/runtime-api

# --------------------------------------------------------------------
# Ollama
# --------------------------------------------------------------------

# Check if Ollama is reachable
ollama-status:
	@echo "Checking Ollama status at $(OLLAMA_HOST)..."
	@curl -s -f http://$(OLLAMA_HOST)/api/tags > /dev/null || (echo "Error: Ollama server not responding at $(OLLAMA_HOST). Start it with 'ollama serve' or check OLLAMA_HOST." && exit 1)
	@echo "Ollama is reachable."

# Pull the Ollama model used by contenox-vibe (default: phi3:3.8b).
start-ollama-pull: ollama-status
	OLLAMA_HOST=$(OLLAMA_HOST) ollama pull $(TASK_MODEL)

# Ensure Ollama is ready: check connection and pull the model.
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

test-vibecli:
	GOMAXPROCS=4 go test -C $(PROJECT_ROOT) -v ./internal/vibecli/...


# --------------------------------------------------------------------
# API tests
# --------------------------------------------------------------------

APITEST_VENV := $(PROJECT_ROOT)/apitests/.venv
APITEST_ACTIVATE := $(APITEST_VENV)/bin/activate

test-api-init:
	test -d $(APITEST_VENV) || python3 -m venv $(APITEST_VENV)
	. $(APITEST_ACTIVATE) && pip install -r $(PROJECT_ROOT)/apitests/requirements.txt

wait-for-server:
	@echo "Waiting for server..."
	@until wget --spider -q http://localhost:8081/health; do \
		echo "Still waiting..."; sleep 2; \
	done

test-api: test-api-init wait-for-server
	. $(APITEST_ACTIVATE) && pytest $(PROJECT_ROOT)/apitests/$(TEST_FILE)

test-api-full: docker-run-runtime test-api


# --------------------------------------------------------------------
# Documentation & Versioning
# --------------------------------------------------------------------

docs-gen:
	go run $(PROJECT_ROOT)/tools/openapi-gen \
		--project="$(PROJECT_ROOT)" \
		--output="$(PROJECT_ROOT)/docs"

docs-markdown: docs-gen
	docker run --rm \
		-v $(PROJECT_ROOT)/docs:/local \
		node:24-alpine sh -c "\
			npm install -g widdershins@4 && \
			widdershins /local/openapi.json -o /local/api-reference.md \
			--summary --resolve --verbose \
		"

docs-html: docs-gen
	cp $(PROJECT_ROOT)/scripts/openapi-rapidoc.html $(PROJECT_ROOT)/docs/openapi.html

set-version:
	go run $(PROJECT_ROOT)/tools/version/main.go set

# Bump version and create release commit + tag. Then: git push && git push origin vX.Y.Z
bump-patch:
	go run $(PROJECT_ROOT)/tools/version/main.go bump patch

bump-minor:
	go run $(PROJECT_ROOT)/tools/version/main.go bump minor

bump-major:
	go run $(PROJECT_ROOT)/tools/version/main.go bump major
vitepress-build:
	cd $(PROJECT_ROOT)/enterprise/vitepress-docs && npm install && npm run docs:build

commit-docs: docs-markdown docs-html vitepress-build
	git add $(PROJECT_ROOT)/docs
	git add $(PROJECT_ROOT)/enterprise/contenox.github.io/docs
	git commit -m "chore: update docs"

release: docs-markdown docs-html set-version
	@echo "Release assets prepared."
