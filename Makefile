# modeld: the native local-inference daemon (cmd/modeld, internal/modeld).
#
# Scope: this Makefile builds/packages/releases ONLY modeld. Everything else in
# this repo (the contenox CLI, website, VS Code extension, Go test suites) is
# owned by Task (Taskfile.yml, run via `task <target>`; see `task --list`).
# modeld is kept on Make because it needs per-OS/per-device native toolchains
# (cmake, a C++ compiler, OpenVINO GenAI, and optionally CUDA/HIP) that a
# plain Go toolchain does not provide, and because the release path assembles
# platforms (windows/darwin) that no single CI/dev box can build in one pass.
PROJECT_ROOT := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))
.DEFAULT_GOAL := help

# Optional local environment. This lets ignored repo-local `.env` files provide
# release-store settings such as MODELD_DEPS_S3_URI and AWS_REGION.
LOCAL_ENV_FILE := $(PROJECT_ROOT).env
ifneq (,$(wildcard $(LOCAL_ENV_FILE)))
include $(LOCAL_ENV_FILE)
export $(shell sed -n 's/^\([A-Za-z_][A-Za-z0-9_]*\)[[:space:]]*=.*/\1/p' $(LOCAL_ENV_FILE))
endif

# Shared native build flags for modeld and native backend tests.
include $(PROJECT_ROOT)mk/openvino-flags.mk
include $(PROJECT_ROOT)mk/llama-flags.mk

# Limit native C++ compile parallelism. Increase on hosts with enough RAM.
MODELD_BUILD_JOBS ?= 4
MODELD_SERVE_ARGS ?=

# llama.cpp runtime library source used by package targets.
# LLAMA_RUNTIME_SRC overrides the autodetected runtime directory.
LLAMA_RUNTIME_LIB_SRC ?= $(if $(strip $(LLAMA_RUNTIME_SRC)),$(LLAMA_RUNTIME_SRC),$(LLAMA_RUNTIME_LIB_DIR))
LLAMA_LIBS_DIR ?= $(PROJECT_ROOT)lib/llamacpp
LLAMA_LIBS_COPY ?=
MODELD_DIST_DIR ?= $(PROJECT_ROOT)bin/dist

# Release packaging.
# MODELD_PLATFORM names the goos-goarch of this host's outputs.
# bundle-modeld-deps writes native dependency bundles under MODELD_DEPS_OUT; they
# are pushed to a store (push-modeld-deps) and consumed by package-modeld-release via
# MODELD_DEPS_ROOT, which links modeld without rebuilding llama.cpp / OpenVINO.
MODELD_PLATFORM ?= $(shell go env GOOS)-$(shell go env GOARCH)
MODELD_DEPS_OUT ?= $(PROJECT_ROOT)bin/modeld-deps
MODELD_DEPS_ROOT ?=
MODELD_PULL_DIR ?= $(MODELD_DEPS_OUT)/pulled
# Artifact store URIs. s3:// uses the aws CLI; any other value is a local directory
# (so the whole push/pull/dedup flow is testable without aws). Dep bundles and final
# modeld packages both live in the store, not GitHub Releases.
MODELD_DEPS_S3_URI ?=
MODELD_RELEASE_S3_URI ?=
MODELD_STORE := bash $(PROJECT_ROOT)scripts/modeld-store.sh
MODELD_RELEASE_DIST_DIR ?= $(PROJECT_ROOT)dist
MODELD_RELEASE_NAME ?= modeld-$(MODELD_VERSION)-$(MODELD_PLATFORM)
MODELD_PROTOCOL_VERSION ?= $(shell sed -n 's/^const ProtocolVersion = //p' $(PROJECT_ROOT)/internal/transport/protocol.go | head -1)
MODELD_MIN_PROTOCOL ?= $(shell sed -n 's/^const MinProtocol = //p' $(PROJECT_ROOT)/internal/transport/protocol.go | head -1)
# Release requires OpenVINO by default; package-modeld-release hard-fails if the
# bundle lacks it. Set MODELD_RELEASE_OPENVINO=0 for llama-only platforms.
MODELD_RELEASE_OPENVINO ?= 1

# Fingerprint profile (modeld-deps-fingerprint). Defaults describe THIS host's
# variant; override to compute the fingerprint of a variant built on another device
# (e.g. a windows/darwin bundle this Linux box cannot build). Build-type/ABI mirror
# Makefile.llamacpp-direct.
MODELD_FP_OPENVINO_WAS_SET := $(filter-out undefined default,$(origin MODELD_FP_OPENVINO))
MODELD_FP_CUDA ?= $(if $(shell command -v nvcc 2>/dev/null),ON,OFF)
MODELD_FP_HIP ?= $(if $(shell command -v hipcc 2>/dev/null),ON,OFF)
# macOS is llama + Metal only; OpenVINO is never part of a darwin build, so its
# fingerprint always uses openvino=0 (matching the darwin producer and package).
MODELD_FP_OPENVINO ?= $(if $(filter darwin%,$(MODELD_PLATFORM)),0,$(if $(MODELD_HAVE_OPENVINO),1,0))
MODELD_FP_BUILD_TYPE ?= Release
MODELD_FP_RUNTIME_ABI ?= dl-v1

# Consumer/preflight profile. Defaults describe the bundle we want to consume, not
# what this checkout can build locally. That lets dev/release machines without
# OpenVINO installed still discover and pull the official OpenVINO bundle.
MODELD_EXPECT_CUDA ?= $(MODELD_FP_CUDA)
MODELD_EXPECT_HIP ?= $(MODELD_FP_HIP)
MODELD_EXPECT_BUILD_TYPE ?= $(MODELD_FP_BUILD_TYPE)
MODELD_EXPECT_RUNTIME_ABI ?= $(MODELD_FP_RUNTIME_ABI)
ifneq ($(MODELD_FP_OPENVINO_WAS_SET),)
MODELD_EXPECT_OPENVINO ?= $(MODELD_FP_OPENVINO)
else
MODELD_EXPECT_OPENVINO ?= $(if $(filter darwin%,$(MODELD_PLATFORM)),0,$(MODELD_RELEASE_OPENVINO))
endif

# modeld release version, stamped into `modeld version`. This is intentionally
# independent from internal/version/version.txt, which belongs to the CLI and
# VS Code extension release cadence.
MODELD_VERSION ?= $(shell tr -d '\r\n' < $(PROJECT_ROOT)/cmd/modeld/version.txt 2>/dev/null)
# cmd/modeld is package main, so the linker binds -X against `main`, not the
# full import path (the import-path form is silently ignored for main packages).
MODELD_VERSION_LD_FLAGS = -X 'main.version=$(MODELD_VERSION)'
MODELD_LLAMA_LD_FLAGS = -X 'github.com/contenox/contenox/internal/modeld/llama.llamaCPPCommit=$(LLAMA_CPP_COMMIT)'
MODELD_OPENVINO_LD_FLAGS = -X 'github.com/contenox/contenox/internal/modeld/openvino.buildTokenizersPath=$(OPENVINO_TOKENIZERS_SO)' -X 'github.com/contenox/contenox/internal/modeld/openvino.buildGenAIVersion=$(OPENVINO_GENAI_VERSION)'

# modeld always includes llama.cpp AND OpenVINO GenAI — both are required
# backends, not optional extras, so build-modeld/package-modeld/run-modeld
# hard-fail via check-modeld-openvino-deps when the OpenVINO SDK isn't set up
# (run `make deps-openvino` first). The only exception is darwin, where
# OpenVINO GenAI is not supported at all (Metal-only; see
# package-modeld-release-darwin below) — that is a platform limitation, not a
# choice to treat OpenVINO as optional. CUDA support follows the llama.cpp
# runtime build. Set CONTENOX_MODELD_BACKEND at runtime to pin backend selection.
MODELD_HAVE_OPENVINO := $(shell test -n "$(strip $(OPENVINO_PKG))" && test -d "$(OPENVINO_GENAI_SRC)/src/cpp/include" && echo 1)

# Per-OS dev/runtime linker flags for OpenVINO (rpath only makes sense on Unix).
ifeq ($(shell go env GOOS 2>/dev/null),windows)
MODELD_OV_DEV_RPATH :=
MODELD_OV_PKG_RPATH :=
else
MODELD_OV_DEV_RPATH = -Wl,-rpath,\$$ORIGIN/modeld-libs
MODELD_OV_PKG_RPATH = -Wl,-rpath,\$$ORIGIN/modeld-libs
endif

ifeq ($(MODELD_HAVE_OPENVINO),1)
MODELD_TAGS := llamanode llamacpp_direct openvino openvino_genai
MODELD_LD_FLAGS := $(MODELD_VERSION_LD_FLAGS) $(MODELD_LLAMA_LD_FLAGS) $(MODELD_OPENVINO_LD_FLAGS)
MODELD_OV_CXXFLAGS = $(OPENVINO_GENAI_CGO_CXXFLAGS)
MODELD_OV_DEV_LDFLAGS = $(OPENVINO_GENAI_LINK_FLAGS) $(MODELD_OV_DEV_RPATH)
MODELD_OV_PKG_LDFLAGS = $(OPENVINO_GENAI_LINK_FLAGS) $(MODELD_OV_PKG_RPATH)
else
MODELD_TAGS := llamanode llamacpp_direct
MODELD_LD_FLAGS := $(MODELD_VERSION_LD_FLAGS) $(MODELD_LLAMA_LD_FLAGS)
MODELD_OV_CXXFLAGS =
MODELD_OV_DEV_LDFLAGS =
MODELD_OV_PKG_LDFLAGS =
endif

.PHONY: help \
	build-llamacpp-runtime build-modeld bundle-modeld-libs bundle-llama-libs package-modeld \
	bundle-modeld-deps bundle-modeld-deps-linux bundle-modeld-deps-darwin bundle-modeld-deps-windows \
	push-modeld-deps pull-modeld-deps push-modeld-release push-modeld-index modeld-release-metadata modeld-deps-fingerprint modeld-deps-profile modeld-deps-pull-dir check-modeld-deps-store check-modeld-deps-bundle deps-modeld-prebuilt package-modeld-prebuilt \
	package-modeld-release package-modeld-release-linux package-modeld-release-darwin package-modeld-release-windows \
	check-modeld-llama-deps check-modeld-openvino-deps \
	clean \
	deps-modeld deps-llamacpp-ref deps-openvino \
	run-modeld \
	test-llamacpp-direct

help:
	@echo "build-*    build-llamacpp-runtime build-modeld"
	@echo "           (build-modeld requires BOTH llama.cpp and OpenVINO GenAI — OpenVINO is not"
	@echo "           optional; it hard-fails via check-modeld-openvino-deps if not set up. Run"
	@echo "           deps-modeld/deps-openvino first. Exception: darwin, where OpenVINO GenAI is"
	@echo "           not supported at all — llama.cpp + Metal only.)"
	@echo "package-*  package-modeld package-modeld-prebuilt package-modeld-release"
	@echo "release-*  bundle-modeld-deps[-linux|-darwin|-windows] push/pull-modeld-deps package-modeld-release[-<os>] modeld-release-metadata push-modeld-release push-modeld-index"
	@echo "           (devices publish native dep bundles; release assembly later pulls a bundle and packages modeld)"
	@echo "test-*     test-llamacpp-direct (delegates to Makefile.llamacpp-direct; see also Makefile.openvino)"
	@echo "run-modeld run modeld from the local build output (CONTENOX_MODELD_BACKEND=llama|openvino to pin backend)"
	@echo "deps-*     deps-modeld deps-modeld-prebuilt deps-llamacpp-ref deps-openvino"
	@echo "           (deps-modeld-prebuilt checks/pulls the expected native dep bundle from the store)"
	@echo "clean      remove modeld native build/runtime outputs"
	@echo "Everything else (contenox CLI, website, VS Code extension, Go test suites) is owned by Task: run 'task --list'."

check-modeld-llama-deps:
	@test -f "$(LLAMA_CPP_REF_DIR)/common/chat.h" || { echo "missing pinned llama.cpp common headers ($(LLAMA_CPP_REF_DIR)) — run: make deps-llamacpp-ref"; exit 1; }
	@ls "$(LLAMA_RUNTIME_LIB_DIR)"/libllama-common.* >/dev/null 2>&1 || ls "$(LLAMA_RUNTIME_LIB_DIR)"/llama-common.dll >/dev/null 2>&1 || ls "$(LLAMA_RUNTIME_LIB_DIR)"/common.dll >/dev/null 2>&1 || { echo "missing direct llama.cpp common library ($(LLAMA_RUNTIME_LIB_DIR)/libllama-common.* / common.dll) — run: make build-llamacpp-runtime"; exit 1; }

# OpenVINO GenAI is a required modeld backend on every platform except darwin
# (Metal-only there; see package-modeld-release-darwin). This hard-fails
# build-modeld/package-modeld instead of silently degrading to a llama-only
# binary when the SDK/headers are missing.
check-modeld-openvino-deps:
	@if [ "$(shell go env GOOS 2>/dev/null)" = "darwin" ]; then \
		echo "check-modeld-openvino-deps: skipped on darwin (OpenVINO GenAI is not supported there; llama.cpp + Metal only)"; \
		exit 0; \
	fi
	@test -n "$(strip $(OPENVINO_PKG))" || { echo "missing OpenVINO SDK (no importable 'openvino' package under $(OPENVINO_VENV)) — OpenVINO GenAI is a required modeld backend, not optional. Run: make deps-openvino"; exit 1; }
	@test -d "$(OPENVINO_GENAI_SRC)/src/cpp/include" || { echo "missing OpenVINO GenAI C++ headers at $(OPENVINO_GENAI_SRC) — OpenVINO GenAI is a required modeld backend, not optional. Run: make deps-openvino"; exit 1; }

build-llamacpp-runtime:
	$(MAKE) -f $(PROJECT_ROOT)Makefile.llamacpp-direct runtime

# OS-aware output name for the dev build of modeld.
MODELD_BIN_NAME := $(if $(filter windows,$(shell go env GOOS 2>/dev/null)),modeld.exe,modeld)

# Build modeld with llama.cpp and OpenVINO GenAI — both required backends.
# Run `make deps-modeld` first (or `make deps-openvino` for just OpenVINO).
build-modeld: build-llamacpp-runtime check-modeld-llama-deps check-modeld-openvino-deps
	@echo "building modeld: tags=[$(MODELD_TAGS)] openvino=$(if $(MODELD_HAVE_OPENVINO),yes,no) target=$(MODELD_BIN_NAME)"
	CGO_ENABLED=1 \
	CGO_CPPFLAGS="$(LLAMA_COMMON_CPPFLAGS) $(LLAMA_DIRECT_CPPFLAGS)" \
	CGO_CXXFLAGS="$(MODELD_OV_CXXFLAGS)" \
	CGO_LDFLAGS="$(LLAMA_DIRECT_LDFLAGS) $(MODELD_OV_DEV_LDFLAGS)" \
	go build -a -p $(MODELD_BUILD_JOBS) -tags '$(MODELD_TAGS)' \
		-ldflags "$(MODELD_LD_FLAGS)" \
		-o "$(PROJECT_ROOT)/bin/$(MODELD_BIN_NAME)" $(PROJECT_ROOT)/cmd/modeld
	@if [ "$(MODELD_HAVE_OPENVINO)" = "1" ]; then \
		COPYFLAG=; \
		if [ "$(shell go env GOOS 2>/dev/null)" = "windows" ]; then COPYFLAG=1; fi; \
		$(MAKE) --no-print-directory MODELD_LIBS_DIR=$(PROJECT_ROOT)/bin/modeld-libs MODELD_LIBS_COPY="$$COPYFLAG" bundle-modeld-libs; \
	fi

# Place OpenVINO runtime libraries next to modeld.
# MODELD_LIBS_COPY=1 copies files instead of symlinking them.
bundle-modeld-libs:
	@rm -rf "$(MODELD_LIBS_DIR)" && mkdir -p "$(MODELD_LIBS_DIR)"
	@genai_lib=$$(find "$(OPENVINO_GENAI_PKG)" -maxdepth 1 \( -name 'libopenvino_genai.so*' -o -name 'libopenvino_genai*.dylib' -o -name 'openvino_genai*.dll' \) | head -1); \
	tokenizers_lib=$$(find "$(OPENVINO_TOKENIZERS_LIB)" -maxdepth 1 \( -name 'libopenvino_tokenizers.so*' -o -name 'libopenvino_tokenizers*.dylib' -o -name 'openvino_tokenizers*.dll' \) | head -1); \
	test -n "$$genai_lib" || { echo "missing openvino_genai runtime library in $(OPENVINO_GENAI_PKG)"; exit 1; }; \
	test -n "$$tokenizers_lib" || { echo "missing openvino_tokenizers runtime library in $(OPENVINO_TOKENIZERS_LIB)"; exit 1; }; \
	if [ -n "$(MODELD_LIBS_COPY)" ]; then \
		cp -L $(OPENVINO_PKG)/libs/* "$(MODELD_LIBS_DIR)/"; \
		cp -L "$$genai_lib" "$(MODELD_LIBS_DIR)/"; \
		cp -L "$$tokenizers_lib" "$(MODELD_LIBS_DIR)/"; \
		echo "bundled OpenVINO runtime (copies) -> $(MODELD_LIBS_DIR)"; \
	else \
		ln -sf $(OPENVINO_PKG)/libs/* "$(MODELD_LIBS_DIR)/"; \
		ln -sf "$$genai_lib" "$(MODELD_LIBS_DIR)/"; \
		ln -sf "$$tokenizers_lib" "$(MODELD_LIBS_DIR)/"; \
		echo "bundled OpenVINO runtime (symlinks) -> $(MODELD_LIBS_DIR)"; \
	fi
	# Stage OV license texts alongside libs for dev packages (compliance).
	@mkdir -p "$(MODELD_LIBS_DIR)/../licenses/openvino" "$(MODELD_LIBS_DIR)/../licenses/openvino-genai" "$(MODELD_LIBS_DIR)/../licenses/openvino-tokenizers" 2>/dev/null || true
	@for d in "$(OPENVINO_PKG)" "$(OPENVINO_GENAI_PKG)" "$(OPENVINO_TOKENIZERS_LIB)"; do \
		for f in LICENSE LICENSE.txt NOTICE COPYING; do \
			[ -f "$$d/$$f" ] && cp -a "$$d/$$f" "$(MODELD_LIBS_DIR)/../licenses/openvino/" 2>/dev/null || true; \
		done; \
	done || true

# Place llama.cpp runtime libraries next to modeld.
bundle-llama-libs:
	@test -n "$(LLAMA_RUNTIME_LIB_SRC)" || { echo "missing direct llama.cpp runtime lib directory. Fetch ref code with: make deps-llamacpp-ref"; exit 1; }
	@test -d "$(LLAMA_RUNTIME_LIB_SRC)" || { echo "direct llama.cpp runtime lib directory does not exist: $(LLAMA_RUNTIME_LIB_SRC)"; exit 1; }
	@{ test -f "$(LLAMA_RUNTIME_LIB_SRC)/libllama.so" || test -f "$(LLAMA_RUNTIME_LIB_SRC)/libllama.dylib" || test -f "$(LLAMA_RUNTIME_LIB_SRC)/libllama.dll" || test -f "$(LLAMA_RUNTIME_LIB_SRC)/llama.dll"; } || { echo "direct llama.cpp runtime at $(LLAMA_RUNTIME_LIB_SRC) does not contain libllama.{so,dylib,dll}"; exit 1; }
	@rm -rf "$(LLAMA_LIBS_DIR)" && mkdir -p "$(dir $(LLAMA_LIBS_DIR))"
	@if [ -n "$(LLAMA_LIBS_COPY)" ]; then \
		mkdir -p "$(LLAMA_LIBS_DIR)"; \
		cp -a "$(LLAMA_RUNTIME_LIB_SRC)"/. "$(LLAMA_LIBS_DIR)/"; \
		echo "bundled direct llama.cpp runtime (copies) -> $(LLAMA_LIBS_DIR)"; \
		if [ "$(LLAMA_CUDA)" = "ON" ]; then \
			bash "$(PROJECT_ROOT)scripts/modeld-vendor-cuda-libs.sh" "$(LLAMA_LIBS_DIR)"; \
			# Stage CUDA EULA if vendored (for compliance in dev packages). \
			mkdir -p "$(LLAMA_LIBS_DIR)/../licenses/cuda" 2>/dev/null || true; \
			for cand in "$${CUDA_HOME:-}/EULA.txt" "$${CUDA_PATH:-}/EULA.txt" /usr/local/cuda/EULA.txt; do \
				[ -f "$$cand" ] && cp -a "$$cand" "$(LLAMA_LIBS_DIR)/../licenses/cuda/" 2>/dev/null || true; \
			done || true; \
		fi; \
	else \
		ln -s "$(LLAMA_RUNTIME_LIB_SRC)" "$(LLAMA_LIBS_DIR)"; \
		echo "bundled direct llama.cpp runtime (symlink) -> $(LLAMA_LIBS_DIR)"; \
	fi

# Build a relocatable modeld bundle under MODELD_DIST_DIR.
# The bundle contains the wrapper, native binary, llama.cpp runtime libraries,
# and OpenVINO libraries when that backend is compiled in.
# CUDA hosts need libcudart.so.12 available to the bundled llama.cpp plugin.
# NOTE: This target is Linux-oriented. On Windows prefer the full release flow
# (bundle-modeld-deps-windows + package-modeld-release-windows using a MODELD_DEPS_ROOT).
package-modeld: build-llamacpp-runtime check-modeld-llama-deps check-modeld-openvino-deps
	@if [ "$(shell go env GOOS 2>/dev/null)" = "windows" ]; then \
		echo "WARNING: package-modeld produces a Linux-style bundle. On Windows use: make package-modeld-release-windows MODELD_DEPS_ROOT=..."; \
	fi
	@rm -rf "$(MODELD_DIST_DIR)" && mkdir -p "$(MODELD_DIST_DIR)"
	@echo "packaging modeld: tags=[$(MODELD_TAGS)] openvino=$(if $(MODELD_HAVE_OPENVINO),yes,no)"
	CGO_ENABLED=1 \
	CGO_CPPFLAGS="$(LLAMA_COMMON_CPPFLAGS) $(LLAMA_DIRECT_CPPFLAGS)" \
	CGO_CXXFLAGS="$(MODELD_OV_CXXFLAGS)" \
	CGO_LDFLAGS="-L$(LLAMA_RUNTIME_LIB_DIR) -Wl,--disable-new-dtags -Wl,-rpath,\$$ORIGIN/lib/llamacpp -Wl,-rpath,\$$ORIGIN/modeld-libs -Wl,-rpath-link,$(LLAMA_RUNTIME_LIB_DIR) $(LLAMA_DIRECT_LINK_LIBS) $(MODELD_OV_PKG_LDFLAGS)" \
	go build -a -p $(MODELD_BUILD_JOBS) -tags '$(MODELD_TAGS)' \
		-ldflags "$(MODELD_LD_FLAGS)" \
		-o "$(MODELD_DIST_DIR)/modeld.bin" $(PROJECT_ROOT)/cmd/modeld
	@{ \
		printf '%s\n' '#!/usr/bin/env sh'; \
		printf '%s\n' 'set -eu'; \
		printf '%s\n' 'SELF_DIR=$$(CDPATH= cd -- "$$(dirname -- "$$0")" && pwd)'; \
		printf '%s\n' 'LIB_DIR="$$SELF_DIR/lib/llamacpp"'; \
		printf '%s\n' 'if [ -d "$$LIB_DIR" ]; then'; \
		printf '%s\n' '  export LD_LIBRARY_PATH="$$LIB_DIR$${LD_LIBRARY_PATH:+:$$LD_LIBRARY_PATH}"'; \
		printf '%s\n' '  export CONTENOX_LLAMA_BACKEND_DIR="$${CONTENOX_LLAMA_BACKEND_DIR:-$$LIB_DIR}"'; \
		printf '%s\n' 'fi'; \
		printf '%s\n' 'exec "$$SELF_DIR/modeld.bin" "$$@"'; \
	} > "$(MODELD_DIST_DIR)/modeld"
	@chmod +x "$(MODELD_DIST_DIR)/modeld"
	@if [ "$(MODELD_HAVE_OPENVINO)" = "1" ]; then $(MAKE) --no-print-directory MODELD_LIBS_DIR="$(MODELD_DIST_DIR)/modeld-libs" MODELD_LIBS_COPY=1 bundle-modeld-libs; fi
	@$(MAKE) --no-print-directory LLAMA_LIBS_DIR="$(MODELD_DIST_DIR)/lib/llamacpp" LLAMA_LIBS_COPY=1 bundle-llama-libs
	@echo "relocatable modeld package -> $(MODELD_DIST_DIR) (run: $(MODELD_DIST_DIR)/modeld serve)"

# Produce a native dependency bundle from this host's build outputs. The bundle is a
# build input for package-modeld-release, not a user-facing package. A device builds
# whatever variant it can compile (CPU / CUDA / HIP / Metal, with or without OpenVINO);
# the bundle name and manifest record the accelerator profile. There is one producer
# per OS (scripts/modeld-deps-bundle-<os>.sh) since the native library names differ;
# run the one matching the build device. Run `make deps-modeld` first for OpenVINO.
MODELD_DEPS_BUNDLE_ENV = PLATFORM="$(MODELD_PLATFORM)" OUT="$(MODELD_DEPS_OUT)" \
	LLAMA_REF="$(LLAMA_CPP_REF_DIR)" LLAMA_RUNTIME="$(LLAMA_RUNTIME_DIR)" \
	LLAMA_CPP_COMMIT="$(LLAMA_CPP_COMMIT)" OPENVINO_PKG="$(OPENVINO_PKG)" \
	GENAI_SRC="$(OPENVINO_GENAI_SRC)" GENAI_PKG="$(OPENVINO_GENAI_PKG)" \
	TOKENIZERS_LIB="$(OPENVINO_TOKENIZERS_LIB)" OPENVINO_GENAI_VERSION="$(OPENVINO_GENAI_VERSION)"

# Dispatch to the producer for the host OS.
bundle-modeld-deps:
	@$(MAKE) --no-print-directory bundle-modeld-deps-$$(go env GOOS)

# Linux builds the runtime first as a convenience; darwin/windows producers consume a
# runtime the device already built (their scripts validate the inputs and fail clearly).
bundle-modeld-deps-linux: build-llamacpp-runtime check-modeld-llama-deps
	@$(MODELD_DEPS_BUNDLE_ENV) bash $(PROJECT_ROOT)scripts/modeld-deps-bundle-linux.sh

bundle-modeld-deps-darwin:
	@$(MODELD_DEPS_BUNDLE_ENV) bash $(PROJECT_ROOT)scripts/modeld-deps-bundle-darwin.sh

bundle-modeld-deps-windows:
	@$(MODELD_DEPS_BUNDLE_ENV) bash $(PROJECT_ROOT)scripts/modeld-deps-bundle-windows.sh

# Print the fingerprint of a bundle's build inputs (pins). Pin-only, so it needs no
# built artifacts. Producer checks can override MODELD_PLATFORM/MODELD_FP_*;
# consumer preflight/pull targets use MODELD_PLATFORM/MODELD_EXPECT_*.
modeld-deps-fingerprint:
	@PLATFORM="$(MODELD_PLATFORM)" \
	LLAMA_CPP_COMMIT="$(LLAMA_CPP_COMMIT)" \
	LLAMA_BUILD_TYPE="$(MODELD_FP_BUILD_TYPE)" \
	LLAMA_RUNTIME_ABI="$(MODELD_FP_RUNTIME_ABI)" \
	CUDA="$(MODELD_FP_CUDA)" HIP="$(MODELD_FP_HIP)" \
	OPENVINO="$(MODELD_FP_OPENVINO)" \
	OPENVINO_GENAI_VERSION="$(OPENVINO_GENAI_VERSION)" \
	bash $(PROJECT_ROOT)scripts/modeld-deps-fingerprint.sh

# Print the consumer-side dependency profile and fingerprint. This is the pre-build
# answer to "which native dep bundle do I need?" and does not require built deps.
modeld-deps-profile:
	@fp=$$(PLATFORM="$(MODELD_PLATFORM)" LLAMA_CPP_COMMIT="$(LLAMA_CPP_COMMIT)" \
		LLAMA_BUILD_TYPE="$(MODELD_EXPECT_BUILD_TYPE)" LLAMA_RUNTIME_ABI="$(MODELD_EXPECT_RUNTIME_ABI)" \
		CUDA="$(MODELD_EXPECT_CUDA)" HIP="$(MODELD_EXPECT_HIP)" OPENVINO="$(MODELD_EXPECT_OPENVINO)" \
		OPENVINO_GENAI_VERSION="$(OPENVINO_GENAI_VERSION)" \
		bash $(PROJECT_ROOT)scripts/modeld-deps-fingerprint.sh); \
	echo "platform=$(MODELD_PLATFORM)"; \
	echo "fingerprint=$$fp"; \
	echo "llama_cpp_commit=$(LLAMA_CPP_COMMIT)"; \
	echo "llama_build_type=$(MODELD_EXPECT_BUILD_TYPE)"; \
	echo "llama_runtime_abi=$(MODELD_EXPECT_RUNTIME_ABI)"; \
	echo "cuda=$(MODELD_EXPECT_CUDA)"; \
	echo "hip=$(MODELD_EXPECT_HIP)"; \
	echo "openvino=$(MODELD_EXPECT_OPENVINO)"; \
	echo "openvino_genai_version=$(OPENVINO_GENAI_VERSION)"; \
	echo "pull_dir=$(MODELD_PULL_DIR)/$(MODELD_PLATFORM)-$$fp"; \
	if [ -n "$(MODELD_DEPS_S3_URI)" ]; then \
		echo "manifest=$(MODELD_DEPS_S3_URI)/$(MODELD_PLATFORM)/$$fp/manifest.json"; \
	else \
		echo "manifest=(set MODELD_DEPS_S3_URI to check a store)"; \
	fi

modeld-deps-pull-dir:
	@fp=$$(PLATFORM="$(MODELD_PLATFORM)" LLAMA_CPP_COMMIT="$(LLAMA_CPP_COMMIT)" \
		LLAMA_BUILD_TYPE="$(MODELD_EXPECT_BUILD_TYPE)" LLAMA_RUNTIME_ABI="$(MODELD_EXPECT_RUNTIME_ABI)" \
		CUDA="$(MODELD_EXPECT_CUDA)" HIP="$(MODELD_EXPECT_HIP)" OPENVINO="$(MODELD_EXPECT_OPENVINO)" \
		OPENVINO_GENAI_VERSION="$(OPENVINO_GENAI_VERSION)" \
		bash $(PROJECT_ROOT)scripts/modeld-deps-fingerprint.sh); \
	echo "$(MODELD_PULL_DIR)/$(MODELD_PLATFORM)-$$fp"

# Preflight the store before building native deps. Exit 0 only when the exact
# expected platform/fingerprint manifest exists.
check-modeld-deps-store:
	@test -n "$(MODELD_DEPS_S3_URI)" || { echo "set MODELD_DEPS_S3_URI=s3://bucket/prefix (or a local dir to test)"; exit 1; }
	@fp=$$(PLATFORM="$(MODELD_PLATFORM)" LLAMA_CPP_COMMIT="$(LLAMA_CPP_COMMIT)" \
		LLAMA_BUILD_TYPE="$(MODELD_EXPECT_BUILD_TYPE)" LLAMA_RUNTIME_ABI="$(MODELD_EXPECT_RUNTIME_ABI)" \
		CUDA="$(MODELD_EXPECT_CUDA)" HIP="$(MODELD_EXPECT_HIP)" OPENVINO="$(MODELD_EXPECT_OPENVINO)" \
		OPENVINO_GENAI_VERSION="$(OPENVINO_GENAI_VERSION)" \
		bash $(PROJECT_ROOT)scripts/modeld-deps-fingerprint.sh); \
	key="$(MODELD_DEPS_S3_URI)/$(MODELD_PLATFORM)/$$fp"; \
	echo "checking modeld deps: platform=$(MODELD_PLATFORM) fingerprint=$$fp"; \
	echo "profile: llama=$(LLAMA_CPP_COMMIT) build=$(MODELD_EXPECT_BUILD_TYPE) abi=$(MODELD_EXPECT_RUNTIME_ABI) cuda=$(MODELD_EXPECT_CUDA) hip=$(MODELD_EXPECT_HIP) openvino=$(MODELD_EXPECT_OPENVINO) genai=$(OPENVINO_GENAI_VERSION)"; \
	if $(MODELD_STORE) exists "$$key/manifest.json"; then \
		echo "available: $$key/manifest.json"; \
		echo "pull: make pull-modeld-deps"; \
	else \
		echo "missing: $$key/manifest.json"; \
		echo "build/push on a capable producer device: make deps-modeld bundle-modeld-deps push-modeld-deps"; \
		exit 1; \
	fi

# Upload native dependency bundles to the store as plain files (no archive), keyed by
# platform/fingerprint. Each device pushes the variants it can build; the union in the
# store lets the release assemble platforms no single device can build (windows/darwin).
# A fingerprint already present is skipped, so we never re-upload a version we have.
push-modeld-deps:
	@test -n "$(MODELD_DEPS_S3_URI)" || { echo "set MODELD_DEPS_S3_URI=s3://bucket/prefix (or a local dir to test)"; exit 1; }
	@found=0; \
	for envf in $(MODELD_DEPS_OUT)/*/bundle.env; do \
		[ -f "$$envf" ] || continue; found=1; \
		bdir=$$(dirname "$$envf"); \
		( . "$$envf"; \
		  key="$(MODELD_DEPS_S3_URI)/$$MODELD_BUNDLE_PLATFORM/$$MODELD_BUNDLE_FINGERPRINT"; \
		  if $(MODELD_STORE) exists "$$key/manifest.json"; then \
		    echo "skip (already in store): $$MODELD_BUNDLE_PLATFORM/$$MODELD_BUNDLE_FINGERPRINT"; \
		  else \
		    echo "put $$bdir -> $$key/"; \
		    $(MODELD_STORE) put "$$bdir" "$$key"; \
		  fi ) || exit 1; \
	done; \
	[ "$$found" = 1 ] || { echo "no bundles in $(MODELD_DEPS_OUT); run: make bundle-modeld-deps"; exit 1; }

# Fetch a native dependency bundle from the store into a local dir for packaging. The
# fingerprint is computed from the expected consumer profile, so override
# MODELD_PLATFORM/MODELD_EXPECT_* to pull a different platform or variant.
pull-modeld-deps:
	@test -n "$(MODELD_DEPS_S3_URI)" || { echo "set MODELD_DEPS_S3_URI=s3://bucket/prefix (or a local dir to test)"; exit 1; }
	@fp=$$(PLATFORM="$(MODELD_PLATFORM)" LLAMA_CPP_COMMIT="$(LLAMA_CPP_COMMIT)" \
		LLAMA_BUILD_TYPE="$(MODELD_EXPECT_BUILD_TYPE)" LLAMA_RUNTIME_ABI="$(MODELD_EXPECT_RUNTIME_ABI)" \
		CUDA="$(MODELD_EXPECT_CUDA)" HIP="$(MODELD_EXPECT_HIP)" OPENVINO="$(MODELD_EXPECT_OPENVINO)" \
		OPENVINO_GENAI_VERSION="$(OPENVINO_GENAI_VERSION)" \
		bash $(PROJECT_ROOT)scripts/modeld-deps-fingerprint.sh); \
	key="$(MODELD_DEPS_S3_URI)/$(MODELD_PLATFORM)/$$fp"; \
	dest="$(MODELD_PULL_DIR)/$(MODELD_PLATFORM)-$$fp"; \
	$(MODELD_STORE) exists "$$key/manifest.json" || { echo "variant not in store: $$key — build+push it on a $(MODELD_PLATFORM) device first"; exit 1; }; \
	$(MODELD_STORE) get "$$key" "$$dest"; \
	echo "pulled $(MODELD_PLATFORM) ($$fp) -> $$dest"; \
	echo "next: make package-modeld-release MODELD_DEPS_ROOT=$$dest"

# Dev/release consumer convenience: preflight, pull, and validate the expected
# prebuilt native dependency bundle without building llama.cpp/OpenVINO locally.
deps-modeld-prebuilt:
	@test -n "$(MODELD_DEPS_S3_URI)" || { echo "set MODELD_DEPS_S3_URI=s3://bucket/prefix (or a local dir to test)"; exit 1; }
	@fp=$$(PLATFORM="$(MODELD_PLATFORM)" LLAMA_CPP_COMMIT="$(LLAMA_CPP_COMMIT)" \
		LLAMA_BUILD_TYPE="$(MODELD_EXPECT_BUILD_TYPE)" LLAMA_RUNTIME_ABI="$(MODELD_EXPECT_RUNTIME_ABI)" \
		CUDA="$(MODELD_EXPECT_CUDA)" HIP="$(MODELD_EXPECT_HIP)" OPENVINO="$(MODELD_EXPECT_OPENVINO)" \
		OPENVINO_GENAI_VERSION="$(OPENVINO_GENAI_VERSION)" \
		bash $(PROJECT_ROOT)scripts/modeld-deps-fingerprint.sh); \
	key="$(MODELD_DEPS_S3_URI)/$(MODELD_PLATFORM)/$$fp"; \
	dest="$(MODELD_PULL_DIR)/$(MODELD_PLATFORM)-$$fp"; \
	echo "checking modeld deps: platform=$(MODELD_PLATFORM) fingerprint=$$fp"; \
	$(MODELD_STORE) exists "$$key/manifest.json" || { echo "prebuilt modeld deps missing: $$key/manifest.json"; echo "build/push on a capable producer device: make deps-modeld bundle-modeld-deps push-modeld-deps"; exit 1; }; \
	$(MODELD_STORE) get "$$key" "$$dest"; \
	$(MAKE) --no-print-directory check-modeld-deps-bundle MODELD_DEPS_ROOT="$$dest" MODELD_RELEASE_OPENVINO="$(MODELD_EXPECT_OPENVINO)"; \
	echo "prebuilt modeld deps ready: MODELD_DEPS_ROOT=$$dest"

# Local/dev packaging path that consumes the prebuilt native dependency bundle
# instead of building heavy C/C++ dependencies on this machine. It does not upload.
package-modeld-prebuilt:
	@test -n "$(MODELD_DEPS_S3_URI)" || { echo "set MODELD_DEPS_S3_URI=s3://bucket/prefix (or a local dir to test)"; exit 1; }
	@fp=$$(PLATFORM="$(MODELD_PLATFORM)" LLAMA_CPP_COMMIT="$(LLAMA_CPP_COMMIT)" \
		LLAMA_BUILD_TYPE="$(MODELD_EXPECT_BUILD_TYPE)" LLAMA_RUNTIME_ABI="$(MODELD_EXPECT_RUNTIME_ABI)" \
		CUDA="$(MODELD_EXPECT_CUDA)" HIP="$(MODELD_EXPECT_HIP)" OPENVINO="$(MODELD_EXPECT_OPENVINO)" \
		OPENVINO_GENAI_VERSION="$(OPENVINO_GENAI_VERSION)" \
		bash $(PROJECT_ROOT)scripts/modeld-deps-fingerprint.sh); \
	dest="$(MODELD_PULL_DIR)/$(MODELD_PLATFORM)-$$fp"; \
	$(MAKE) --no-print-directory deps-modeld-prebuilt; \
	$(MAKE) --no-print-directory package-modeld-release MODELD_DEPS_ROOT="$$dest" MODELD_RELEASE_OPENVINO="$(MODELD_EXPECT_OPENVINO)"

# Upload final modeld packages to the store, keyed by version. Final binaries live in
# the store (S3), not GitHub Releases.
push-modeld-release: modeld-release-metadata
	@test -n "$(MODELD_RELEASE_S3_URI)" || { echo "set MODELD_RELEASE_S3_URI=s3://bucket/prefix (or a local dir to test)"; exit 1; }
	@set -- $(MODELD_RELEASE_DIST_DIR)/modeld-$(MODELD_VERSION)-*.tar.gz $(MODELD_RELEASE_DIST_DIR)/modeld-$(MODELD_VERSION)-*.zip; \
	found=0; \
	for f do \
		[ -f "$$f" ] || continue; found=1; \
		[ -f "$$f.sha256" ] || { echo "missing checksum for $$f; run: make package-modeld-release"; exit 1; }; \
		[ -f "$$f.build.json" ] || { echo "missing release metadata for $$f; run: make package-modeld-release with the updated packaging scripts"; exit 1; }; \
	done; \
	[ "$$found" = 1 ] || { echo "no modeld $(MODELD_VERSION) packages in $(MODELD_RELEASE_DIST_DIR); run: make package-modeld-release"; exit 1; }; \
	for f do \
		[ -f "$$f" ] || continue; \
		base=$$(basename "$$f"); dest="$(MODELD_RELEASE_S3_URI)/$(MODELD_VERSION)"; \
		echo "cp $$f -> $$dest/$$base"; \
		$(MODELD_STORE) cp "$$f" "$$dest/$$base" || exit 1; \
		$(MODELD_STORE) cp "$$f.sha256" "$$dest/$$base.sha256" || exit 1; \
		$(MODELD_STORE) cp "$$f.build.json" "$$dest/$$base.build.json" || exit 1; \
	done; \
	$(MAKE) --no-print-directory push-modeld-index

push-modeld-index:
	@test -n "$(MODELD_RELEASE_S3_URI)" || { echo "set MODELD_RELEASE_S3_URI=s3://bucket/prefix (or a local dir to test)"; exit 1; }
	@bash $(PROJECT_ROOT)scripts/modeld-index-refresh.sh "$(MODELD_RELEASE_S3_URI)"

modeld-release-metadata:
	@set -- $(MODELD_RELEASE_DIST_DIR)/modeld-$(MODELD_VERSION)-*.tar.gz $(MODELD_RELEASE_DIST_DIR)/modeld-$(MODELD_VERSION)-*.zip; \
	found=0; \
	for f do [ ! -f "$$f" ] || found=1; done; \
	[ "$$found" = 1 ] || { echo "no modeld $(MODELD_VERSION) packages in $(MODELD_RELEASE_DIST_DIR); run: make package-modeld-release"; exit 1; }; \
	MODELD_RELEASE_PROTOCOL="$(MODELD_MIN_PROTOCOL)" bash $(PROJECT_ROOT)scripts/modeld-release-metadata.sh "$$@"

# Validate that an extracted dependency bundle has everything the release link needs.
# Hard-fails when OpenVINO is required but the bundle does not declare/contain it, so
# a release can never silently fall back to a reduced backend set.
check-modeld-deps-bundle:
	@test -n "$(MODELD_DEPS_ROOT)" || { echo "set MODELD_DEPS_ROOT=/path/to/modeld-deps-<platform>"; exit 1; }
	@test -f "$(MODELD_DEPS_ROOT)/manifest.json" || { echo "bundle missing manifest.json: $(MODELD_DEPS_ROOT)"; exit 1; }
	@test -f "$(MODELD_DEPS_ROOT)/llama/ref/common/chat.h" || { echo "bundle missing llama.cpp ref headers"; exit 1; }
	@ls "$(MODELD_DEPS_ROOT)"/llama/runtime/lib/libllama.* >/dev/null 2>&1 || ls "$(MODELD_DEPS_ROOT)"/llama/runtime/lib/llama.dll >/dev/null 2>&1 || { echo "bundle missing llama runtime lib (libllama.{so,dylib,dll})"; exit 1; }
	@ls "$(MODELD_DEPS_ROOT)"/llama/runtime/lib/libllama-common.* >/dev/null 2>&1 || ls "$(MODELD_DEPS_ROOT)"/llama/runtime/lib/llama-common.dll >/dev/null 2>&1 || ls "$(MODELD_DEPS_ROOT)"/llama/runtime/lib/common.dll >/dev/null 2>&1 || { echo "bundle missing llama common runtime lib (libllama-common.{so,dylib} / llama-common.dll / common.dll)"; exit 1; }
	@if [ "$(MODELD_RELEASE_OPENVINO)" = "1" ]; then \
		grep -q '"openvino": *true' "$(MODELD_DEPS_ROOT)/manifest.json" || { echo "MODELD_RELEASE_OPENVINO=1 but bundle manifest does not declare openvino:true (refusing to silently drop OpenVINO)"; exit 1; }; \
		test -d "$(MODELD_DEPS_ROOT)/openvino/genai/src/cpp/include" || { echo "bundle missing OpenVINO GenAI headers"; exit 1; }; \
		ls "$(MODELD_DEPS_ROOT)"/openvino/genai/*openvino_genai* >/dev/null 2>&1 || { echo "bundle missing openvino_genai lib"; exit 1; }; \
		ls "$(MODELD_DEPS_ROOT)"/openvino/tokenizers/lib/*openvino_tokenizers* >/dev/null 2>&1 || { echo "bundle missing openvino_tokenizers lib"; exit 1; }; \
		ls "$(MODELD_DEPS_ROOT)"/openvino/openvino/libs/*openvino.* >/dev/null 2>&1 || { echo "bundle missing openvino runtime lib"; exit 1; }; \
	fi
	@if grep -q '"cuda": *true' "$(MODELD_DEPS_ROOT)/manifest.json"; then \
		for lib in libcudart libcublas libcublasLt; do \
			ls "$(MODELD_DEPS_ROOT)"/llama/runtime/lib/$$lib.so.* >/dev/null 2>&1 || { echo "bundle manifest declares cuda:true but is missing vendored $$lib (a CUDA-driver-only target host would silently fall back to CPU); rebuild with the fixed bundle-llama-libs/modeld-deps-bundle-linux.sh"; exit 1; }; \
		done; \
	fi
	@echo "modeld deps bundle OK: $(MODELD_DEPS_ROOT) (openvino required=$(MODELD_RELEASE_OPENVINO))"
	# License completeness check for public redistribution (VSCode, registries, Store).
	@test -d "$(MODELD_DEPS_ROOT)/licenses" || { echo "bundle missing licenses/ dir"; exit 1; }
	@ls "$(MODELD_DEPS_ROOT)/licenses"/* >/dev/null 2>&1 || { echo "bundle licenses/ appears empty"; exit 1; }
	@if [ "$(MODELD_RELEASE_OPENVINO)" = "1" ]; then \
		ls "$(MODELD_DEPS_ROOT)/licenses/openvino"*/* 2>/dev/null | head -1 >/dev/null || { echo "openvino declared but no license texts under licenses/openvino*"; exit 1; }; \
	fi
	@echo "licenses/ present for declared components"

# Package a release modeld bundle by linking against an extracted native dependency
# bundle (MODELD_DEPS_ROOT) instead of rebuilding native deps. There is one target per
# OS (package-modeld-release-<os>); package-modeld-release dispatches to the host OS.
# The root paths/tags are re-pointed at the bundle via target-specific variables shared
# by all OSes; only the link flags, wrapper, and archive differ per OS. Deterministic
# and self-contained: it does not rebuild llama.cpp/OpenVINO and refuses to drop a
# backend. Linux is verified end-to-end; Darwin follows ld64 @loader_path, and Windows
# supports either MinGW-style DLL linking or Clang/MSVC import libraries selected by
# MODELD_WINDOWS_TOOLCHAIN.
MODELD_RELEASE_OSES := linux darwin windows
MODELD_RELEASE_TARGETS := $(addprefix package-modeld-release-,$(MODELD_RELEASE_OSES))

# Per-OS pieces, selected in the recipe as $(VAR_$*).
MODELD_PKG_RPATH_linux   = -Wl,--disable-new-dtags -Wl,-rpath,\$$ORIGIN/lib/llamacpp -Wl,-rpath,\$$ORIGIN/modeld-libs -Wl,-rpath-link,$(LLAMA_RUNTIME_LIB_DIR)
MODELD_PKG_RPATH_darwin  = -Wl,-rpath,@loader_path/lib/llamacpp -Wl,-rpath,@loader_path/modeld-libs
MODELD_PKG_RPATH_windows =
MODELD_WINDOWS_TOOLCHAIN ?= mingw
MODELD_PKG_LLAMA_LIBS_linux   = $(LLAMA_DIRECT_LINK_LIBS)
MODELD_PKG_LLAMA_LIBS_darwin  = -lcommon -lllama -lggml -lggml-base -lstdc++
MODELD_PKG_LLAMA_LIBS_windows_mingw = -lllama-common -lllama -lggml -lggml-base -lstdc++ -static-libgcc -static-libstdc++
MODELD_PKG_LLAMA_LIBS_windows_msvc = $(LLAMA_RUNTIME_LIB_DIR)/llama-common.lib $(LLAMA_RUNTIME_LIB_DIR)/llama.lib $(LLAMA_RUNTIME_LIB_DIR)/ggml.lib $(LLAMA_RUNTIME_LIB_DIR)/ggml-base.lib
MODELD_PKG_LLAMA_LIBS_windows = $(MODELD_PKG_LLAMA_LIBS_windows_$(MODELD_WINDOWS_TOOLCHAIN))
MODELD_PKG_OV_LIBS_linux   = $(if $(filter 1,$(MODELD_RELEASE_OPENVINO)),$(OPENVINO_GENAI_LINK_FLAGS),)
MODELD_PKG_OV_LIBS_darwin  = $(if $(filter 1,$(MODELD_RELEASE_OPENVINO)),-L$(OPENVINO_PKG)/libs -L$(OPENVINO_GENAI_PKG) -L$(OPENVINO_TOKENIZERS_LIB) -lopenvino_genai -lopenvino -lstdc++,)
MODELD_PKG_OV_LIBS_windows_mingw = $(if $(filter 1,$(MODELD_RELEASE_OPENVINO)),-L$(OPENVINO_PKG)/libs -L$(OPENVINO_GENAI_PKG) -L$(OPENVINO_TOKENIZERS_LIB) -l:openvino_genai.dll -l:openvino.dll,)
MODELD_PKG_OV_LIBS_windows_msvc = $(if $(filter 1,$(MODELD_RELEASE_OPENVINO)),$(OPENVINO_GENAI_PKG)/openvino_genai.lib $(OPENVINO_PKG)/libs/openvino.lib,)
MODELD_PKG_OV_LIBS_windows = $(MODELD_PKG_OV_LIBS_windows_$(MODELD_WINDOWS_TOOLCHAIN))
MODELD_PKG_OPENVINO_LD_FLAGS_linux = -X 'github.com/contenox/contenox/internal/modeld/openvino.buildTokenizersPath=$(OPENVINO_TOKENIZERS_LIB)/libopenvino_tokenizers.so' -X 'github.com/contenox/contenox/internal/modeld/openvino.buildGenAIVersion=$(OPENVINO_GENAI_VERSION)'
MODELD_PKG_OPENVINO_LD_FLAGS_darwin = -X 'github.com/contenox/contenox/internal/modeld/openvino.buildTokenizersPath=$(OPENVINO_TOKENIZERS_LIB)/libopenvino_tokenizers.dylib' -X 'github.com/contenox/contenox/internal/modeld/openvino.buildGenAIVersion=$(OPENVINO_GENAI_VERSION)'
MODELD_PKG_OPENVINO_LD_FLAGS_windows = -X 'github.com/contenox/contenox/internal/modeld/openvino.buildTokenizersPath=$(OPENVINO_TOKENIZERS_LIB)/openvino_tokenizers.dll' -X 'github.com/contenox/contenox/internal/modeld/openvino.buildGenAIVersion=$(OPENVINO_GENAI_VERSION)'
MODELD_PKG_BIN_linux   = modeld.bin
MODELD_PKG_BIN_darwin  = modeld.bin
MODELD_PKG_BIN_windows = modeld.exe
MODELD_PKG_LAUNCHER_linux   = modeld
MODELD_PKG_LAUNCHER_darwin  = modeld
MODELD_PKG_LAUNCHER_windows = modeld.cmd
MODELD_MSVC_REDIST_DIR ?=
# vcomp140.dll (MSVC OpenMP) is a load-time import of ggml-base.dll, so it is
# required for the daemon to start, not optional.
MODELD_MSVC_REDIST_DLLS ?= msvcp140.dll vcruntime140.dll vcruntime140_1.dll vcomp140.dll

# Dispatch to the packager for the host OS.
package-modeld-release:
	@$(MAKE) --no-print-directory package-modeld-release-$$(go env GOOS) MODELD_DEPS_ROOT="$(MODELD_DEPS_ROOT)"

# Bundle-path/tag overrides shared by every per-OS target.
$(MODELD_RELEASE_TARGETS): MODELD_DIST_DIR := $(MODELD_RELEASE_DIST_DIR)/$(MODELD_RELEASE_NAME)
$(MODELD_RELEASE_TARGETS): LLAMA_CPP_REF_DIR := $(abspath $(MODELD_DEPS_ROOT)/llama/ref)
$(MODELD_RELEASE_TARGETS): LLAMA_RUNTIME_DIR := $(abspath $(MODELD_DEPS_ROOT)/llama/runtime)
$(MODELD_RELEASE_TARGETS): LLAMA_RUNTIME_LIB_DIR := $(abspath $(MODELD_DEPS_ROOT)/llama/runtime/lib)
$(MODELD_RELEASE_TARGETS): OPENVINO_PKG := $(abspath $(MODELD_DEPS_ROOT)/openvino/openvino)
$(MODELD_RELEASE_TARGETS): OPENVINO_GENAI_SRC := $(abspath $(MODELD_DEPS_ROOT)/openvino/genai)
$(MODELD_RELEASE_TARGETS): OPENVINO_GENAI_PKG := $(abspath $(MODELD_DEPS_ROOT)/openvino/genai)
$(MODELD_RELEASE_TARGETS): OPENVINO_TOKENIZERS_LIB := $(abspath $(MODELD_DEPS_ROOT)/openvino/tokenizers/lib)
# Recursive (=) so they honor a per-target MODELD_RELEASE_OPENVINO (darwin sets 0).
$(MODELD_RELEASE_TARGETS): MODELD_TAGS = $(if $(filter 1,$(MODELD_RELEASE_OPENVINO)),llamanode llamacpp_direct openvino openvino_genai,llamanode llamacpp_direct)
$(MODELD_RELEASE_TARGETS): MODELD_OV_CXXFLAGS = $(if $(filter 1,$(MODELD_RELEASE_OPENVINO)),$(OPENVINO_GENAI_CGO_CXXFLAGS),)
# Apple Silicon is llama + Metal; OpenVINO GenAI is not supported there, so the darwin
# package never requires/links OpenVINO (override on the command line if that changes).
package-modeld-release-darwin: MODELD_RELEASE_OPENVINO := 0

$(MODELD_RELEASE_TARGETS): package-modeld-release-%: check-modeld-deps-bundle
	@rm -rf "$(MODELD_DIST_DIR)" && mkdir -p "$(MODELD_DIST_DIR)"
	@echo "packaging modeld release ($*): $(MODELD_RELEASE_NAME) tags=[$(MODELD_TAGS)] openvino=$(MODELD_RELEASE_OPENVINO)"
	CGO_ENABLED=1 \
	CGO_CPPFLAGS="$(if $(filter windows%,$*), -I$(shell cygpath -w $(LLAMA_CPP_REF_DIR)/common) -I$(shell cygpath -w $(LLAMA_CPP_REF_DIR)/vendor) -I$(shell cygpath -w $(LLAMA_RUNTIME_DIR)/include), $(LLAMA_COMMON_CPPFLAGS) $(LLAMA_DIRECT_CPPFLAGS))" \
	CGO_CXXFLAGS="$(MODELD_OV_CXXFLAGS)" \
	CGO_LDFLAGS="-L$(LLAMA_RUNTIME_LIB_DIR) $(MODELD_PKG_RPATH_$*) $(MODELD_PKG_LLAMA_LIBS_$*) $(MODELD_PKG_OV_LIBS_$*)" \
	go build -a -p $(MODELD_BUILD_JOBS) -tags '$(MODELD_TAGS)' \
		-ldflags "$(MODELD_VERSION_LD_FLAGS) $(MODELD_LLAMA_LD_FLAGS) $(if $(filter 1,$(MODELD_RELEASE_OPENVINO)),$(MODELD_PKG_OPENVINO_LD_FLAGS_$*),)" \
		-o "$(MODELD_DIST_DIR)/$(MODELD_PKG_BIN_$*)" $(PROJECT_ROOT)/cmd/modeld
	@if [ "$*" = "windows" ]; then \
		{ printf '%s\r\n' '@echo off'; \
		  printf '%s\r\n' 'set "SELF=%~dp0"'; \
		  printf '%s\r\n' 'set "PATH=%SELF%lib\llamacpp;%SELF%modeld-libs;%PATH%"'; \
		  printf '%s\r\n' '"%SELF%modeld.exe" %*'; \
		} > "$(MODELD_DIST_DIR)/modeld.cmd"; \
	else \
		{ printf '%s\n' '#!/usr/bin/env sh'; \
		  printf '%s\n' 'set -eu'; \
		  printf '%s\n' 'SELF_DIR=$$(CDPATH= cd -- "$$(dirname -- "$$0")" && pwd)'; \
		  printf '%s\n' 'LIB_DIR="$$SELF_DIR/lib/llamacpp"'; \
		  printf '%s\n' 'if [ -d "$$LIB_DIR" ]; then'; \
		  printf '%s\n' '  export LD_LIBRARY_PATH="$$LIB_DIR$${LD_LIBRARY_PATH:+:$$LD_LIBRARY_PATH}"'; \
		  printf '%s\n' '  export DYLD_LIBRARY_PATH="$$LIB_DIR$${DYLD_LIBRARY_PATH:+:$$DYLD_LIBRARY_PATH}"'; \
		  printf '%s\n' '  export CONTENOX_LLAMA_BACKEND_DIR="$${CONTENOX_LLAMA_BACKEND_DIR:-$$LIB_DIR}"'; \
		  printf '%s\n' 'fi'; \
		  printf '%s\n' 'exec "$$SELF_DIR/$(MODELD_PKG_BIN_$*)" "$$@"'; \
		} > "$(MODELD_DIST_DIR)/modeld"; \
		chmod +x "$(MODELD_DIST_DIR)/modeld"; \
	fi
	@if [ "$(MODELD_RELEASE_OPENVINO)" = "1" ]; then $(MAKE) --no-print-directory \
		OPENVINO_PKG="$(OPENVINO_PKG)" OPENVINO_GENAI_PKG="$(OPENVINO_GENAI_PKG)" \
		OPENVINO_TOKENIZERS_LIB="$(OPENVINO_TOKENIZERS_LIB)" \
		MODELD_LIBS_DIR="$(MODELD_DIST_DIR)/modeld-libs" MODELD_LIBS_COPY=1 bundle-modeld-libs; fi
	@$(MAKE) --no-print-directory LLAMA_RUNTIME_LIB_SRC="$(LLAMA_RUNTIME_LIB_DIR)" LLAMA_LIBS_DIR="$(MODELD_DIST_DIR)/lib/llamacpp" LLAMA_LIBS_COPY=1 bundle-llama-libs
	@if [ "$*" = "windows" ] && [ -n "$(MODELD_MSVC_REDIST_DIR)" ]; then \
		test -d "$(MODELD_MSVC_REDIST_DIR)" || { echo "MODELD_MSVC_REDIST_DIR does not exist: $(MODELD_MSVC_REDIST_DIR)"; exit 1; }; \
		mkdir -p "$(MODELD_DIST_DIR)/modeld-libs"; \
		for dll in $(MODELD_MSVC_REDIST_DLLS); do \
			test -f "$(MODELD_MSVC_REDIST_DIR)/$$dll" || { echo "missing MSVC redistributable DLL: $(MODELD_MSVC_REDIST_DIR)/$$dll"; exit 1; }; \
			cp -a "$(MODELD_MSVC_REDIST_DIR)/$$dll" "$(MODELD_DIST_DIR)/modeld-libs/"; \
		done; \
	fi
	@if [ -d "$(MODELD_DEPS_ROOT)/licenses" ]; then rm -rf "$(MODELD_DIST_DIR)/LICENSES"; cp -a "$(MODELD_DEPS_ROOT)/licenses" "$(MODELD_DIST_DIR)/LICENSES"; fi
	# Ensure at minimum the project LICENSE and any vendored upstream are present (for public distribution compliance).
	@mkdir -p "$(MODELD_DIST_DIR)/LICENSES"
	@[ -f LICENSE ] && cp -a LICENSE "$(MODELD_DIST_DIR)/LICENSES/contenox-LICENSE" 2>/dev/null || true
	@echo "LICENSES/ prepared in $(MODELD_DIST_DIR) (openvino=$(MODELD_RELEASE_OPENVINO) cuda=$(grep -q cuda=ON $(MODELD_DEPS_ROOT)/llama/runtime/.contenox-runtime-stamp 2>/dev/null && echo yes || echo no))"
	@DIST_DIR="$(MODELD_DIST_DIR)" RELEASE_OUT="$(MODELD_RELEASE_DIST_DIR)" \
	NAME="$(MODELD_RELEASE_NAME)" VERSION="$(MODELD_VERSION)" PLATFORM="$(MODELD_PLATFORM)" \
	MIN_PROTOCOL="$(MODELD_MIN_PROTOCOL)" PROTOCOL_VERSION="$(MODELD_PROTOCOL_VERSION)" \
	EXPECT_OPENVINO="$(MODELD_RELEASE_OPENVINO)" TARGET_OS="$*" LAUNCHER="$(MODELD_PKG_LAUNCHER_$*)" \
	MODELD_PACKAGE_DEFER_SMOKE="$(MODELD_PACKAGE_DEFER_SMOKE)" \
	MODELD_PACKAGE_REPORT_FILE="$(MODELD_PACKAGE_REPORT_FILE)" \
	bash $(PROJECT_ROOT)scripts/modeld-package-release.sh

# Run modeld from the local build output.
# Set CONTENOX_MODELD_BACKEND=llama|openvino to pin backend selection.
run-modeld: build-modeld
	CONTENOX_LLAMA_BACKEND_DIR=$(LLAMA_RUNTIME_LIB_DIR) \
	$(PROJECT_ROOT)/bin/modeld serve $(MODELD_SERVE_ARGS)

# deps
# Native backend dependencies for build-modeld.
deps-modeld: deps-llamacpp-ref deps-openvino

deps-llamacpp-ref:
	$(MAKE) -f $(PROJECT_ROOT)Makefile.llamacpp-direct deps-ref

deps-openvino:
	$(MAKE) -f $(PROJECT_ROOT)Makefile.openvino deps-genai genai-src

test-llamacpp-direct:
	$(MAKE) -f $(PROJECT_ROOT)Makefile.llamacpp-direct test

clean:
	rm -rf $(PROJECT_ROOT)/bin/modeld $(PROJECT_ROOT)/bin/modeld.exe $(PROJECT_ROOT)/bin/modeld-libs \
		$(PROJECT_ROOT)/bin/dist $(PROJECT_ROOT)/bin/modeld-deps \
		$(PROJECT_ROOT)/lib/llamacpp $(PROJECT_ROOT).llamacpp-runtime $(PROJECT_ROOT).build/llamacpp $(PROJECT_ROOT).openvino
	@rmdir $(PROJECT_ROOT)/lib 2>/dev/null || true
