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

export EMBED_MODEL EMBED_PROVIDER EMBED_MODEL_CONTEXT_LENGTH
export TASK_MODEL TASK_MODEL_CONTEXT_LENGTH TASK_PROVIDER
export CHAT_MODEL CHAT_MODEL_CONTEXT_LENGTH CHAT_PROVIDER
export TENANCY

# Allow user override of COMPOSE_CMD
COMPOSE_CMD ?= docker compose -f compose.yaml -f compose.local.yaml

.PHONY: run up down build clear logs \
        build-vibe run-vibe vibe-ollama-pull vibe-ollama-ready \
        test test-unit test-system \
        test-api test-api-full test-api-init wait-for-server \
        docs-gen docs-markdown docs-html \
        set-version bump-major bump-minor bump-patch \
        commit-docs release


# --------------------------------------------------------------------
# Runtime lifecycle
# --------------------------------------------------------------------

build:
	$(COMPOSE_CMD) build --build-arg TENANCY=$(TENANCY)

up:
	$(COMPOSE_CMD) up -d

run: down build up

down:
	$(COMPOSE_CMD) down

clear:
	$(COMPOSE_CMD) down --volumes --remove-orphans

logs:
	$(COMPOSE_CMD) logs -f runtime

# Contenox Vibe: single binary, SQLite + in-memory bus + estimate tokenizer
build-vibe:
	go build -o $(PROJECT_ROOT)/bin/contenox-vibe $(PROJECT_ROOT)/cmd/vibe

# Run the vibe binary (builds if needed). Example: make run-vibe ARGS="-input 'hello'"
run-vibe: build-vibe
	$(PROJECT_ROOT)/bin/contenox-vibe $(ARGS)

# Pull the Ollama model used by contenox-vibe (default: phi3:3.8b). Start Ollama first if needed: ollama serve
vibe-ollama-pull:
	ollama pull $(TASK_MODEL)

# Ensure Ollama is ready for contenox-vibe: pull the model. Run "ollama serve" in another terminal if the server is not running.
vibe-ollama-ready: vibe-ollama-pull
	@echo "Model $(TASK_MODEL) ready. If you see 'connection refused', start Ollama in another terminal: ollama serve"


# --------------------------------------------------------------------
# Tests
# --------------------------------------------------------------------

test:
	GOMAXPROCS=4 go test -C $(PROJECT_ROOT) ./...

test-unit:
	GOMAXPROCS=4 go test -C $(PROJECT_ROOT) -run '^TestUnit_' ./...

test-system:
	GOMAXPROCS=4 go test -C $(PROJECT_ROOT) -run '^TestSystem_' ./...


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

test-api-full: run test-api


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
		node:18-alpine sh -c "\
			npm install -g widdershins@4 && \
			widdershins /local/openapi.json -o /local/api-reference.md \
			--summary --resolve --verbose \
		"

docs-html: docs-gen
	cp $(PROJECT_ROOT)/scripts/openapi-rapidoc.html $(PROJECT_ROOT)/docs/openapi.html

set-version:
	go run $(PROJECT_ROOT)/tools/version/main.go set

commit-docs: docs-markdown docs-html
	git add $(PROJECT_ROOT)/docs
	git commit -m "Update API reference"

release: docs-markdown docs-html set-version
	@echo "Release assets prepared."
