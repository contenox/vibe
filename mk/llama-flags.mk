# Shared llama.cpp build configuration.
# Requires PROJECT_ROOT from the including Makefile.

LLAMA_CPP_COMMIT ?= 86b94708f22478f900b76ca02e316f4f3418faff

# Pinned upstream llama.cpp source checkout.
LLAMA_CPP_REF_REPO ?= https://github.com/ggml-org/llama.cpp.git
LLAMA_CPP_REF_DIR ?= $(PROJECT_ROOT)tmp/ref/llama.cpp

# Generated llama.cpp runtime output.
LLAMA_RUNTIME_ROOT ?= $(PROJECT_ROOT).llamacpp-runtime
LLAMA_RUNTIME_PROFILE ?= local
LLAMA_RUNTIME_DIR ?= $(LLAMA_RUNTIME_ROOT)/$(LLAMA_RUNTIME_PROFILE)
LLAMA_RUNTIME_LIB_DIR ?= $(LLAMA_RUNTIME_DIR)/lib

# Detect target OS for layout and link flags. Native Windows builds (under
# MinGW) produce .dll files; Linux produces .so. We use the same
# convention for a "local" dev build as the release packaging expects.
LLAMA_TARGET_OS ?= $(shell go env GOOS 2>/dev/null)
ifeq ($(LLAMA_TARGET_OS),windows)
LLAMA_DIRECT_CPPFLAGS = -I$(abspath $(LLAMA_RUNTIME_DIR)/include)
LLAMA_COMMON_CPPFLAGS = -I$(abspath $(LLAMA_CPP_REF_DIR)/common) -I$(abspath $(LLAMA_CPP_REF_DIR)/vendor)
# For MinGW builds the DLLs live next to the exe or in lib/; use the names
# the windows bundle script and package step look for. The -l:foo.dll form
# tells the linker to use the exact DLL (MinGW supports it for import).
LLAMA_DIRECT_LINK_LIBS = -lllama-common -lllama -lmtmd -lggml -lggml-base -lstdc++
LLAMA_DIRECT_LDFLAGS = -L$(LLAMA_RUNTIME_LIB_DIR) $(LLAMA_DIRECT_LINK_LIBS)
else
LLAMA_DIRECT_CPPFLAGS = -I$(LLAMA_RUNTIME_DIR)/include
LLAMA_COMMON_CPPFLAGS = -I$(LLAMA_CPP_REF_DIR)/common -I$(LLAMA_CPP_REF_DIR)/vendor
# Link modeld against the llama.cpp core libraries.
# Runtime plugins are loaded from CONTENOX_LLAMA_BACKEND_DIR.
LLAMA_DIRECT_LINK_LIBS = -l:libllama-common.so -l:libllama.so -l:libmtmd.so -l:libggml.so -l:libggml-base.so -lstdc++ -lm -ldl -lpthread
LLAMA_DIRECT_LDFLAGS = -L$(LLAMA_RUNTIME_LIB_DIR) -Wl,--disable-new-dtags -Wl,-rpath,$(LLAMA_RUNTIME_LIB_DIR) -Wl,-rpath-link,$(LLAMA_RUNTIME_LIB_DIR) $(LLAMA_DIRECT_LINK_LIBS)
endif
