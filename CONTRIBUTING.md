# Contributing to Contenox

Thank you for your interest in contributing to Contenox! Whether you want to fix a bug, improve documentation, or propose a new feature, your help is welcome.

## Code of Conduct

Please treat all contributors with respect. Engage in constructive discussions and assume good intentions.

## Repository structure

The **`contenox`** binary is the main entrypoint: plans, chat, and other CLI commands, plus **`contenox beam`**, which runs the HTTP server that **serves the Beam web UI** (embedded in the binary from `internal/web/beam/dist`) and backs it with the same engine.

### Makefile overview

The root **`Makefile`** groups targets by prefix:

| Prefix | Purpose |
|--------|---------|
| **`build-*`** | `build-cli`, `build-web` |
| **`test-*`** | Go tests, CLI help check, HTTP API tests, OpenAPI client codegen smoke |
| **`docs-*`** | Regenerate OpenAPI and docs artifacts |
| **`dev-*`** | Local CLI install symlink, Air reload, Vite, `wait-http-ready` |
| **`deps-*`** | `deps-npm`, `deps-go-watch` (Air) |
| **`lint-web`** / **`format-web`** | Beam UI package |
| **`clean`** | Remove frontend `node_modules` / build outputs (see target for details) |

Run **`make help`** at the repo root for the full list (default goal).

Version bumps and release notes for maintainers live in **`Makefile.version`** (`make -f Makefile.version help`).

## Local development setup

### Prerequisites

- [Go](https://go.dev/doc/install) 1.24+
- Access to an LLM provider (e.g. OpenAI API key, or locally via [Ollama](https://ollama.ai/), [vLLM](https://docs.vllm.ai/), etc.)
- `make`
- **Beam UI:** Node.js and npm — run **`make deps-npm`** at the repo root (workspaces under `packages/beam` and `packages/ui`)
- **HTTP API tests (Python):** Python 3 — **`make test-http-api-venv`** creates `apitests/.venv` and installs `apitests/requirements.txt`
- **Optional:** [Air](https://github.com/air-verse/air) for Go live reload (`make deps-go-watch`), `curl` (health checks), Docker (optional for `make docs-markdown`; an `npx` fallback exists)

### Building the CLI

```bash
# Build the binary into ./bin/contenox
make build-cli

# Run an example
./bin/contenox "list files in my home directory"
```

### Building with local LLM inference (CGo)

The `internal/modelrepo/local` package embeds llama.cpp inference directly into the binary via `github.com/ollama/ollama/llama` (CGo). This is **required** to build the full binary — `CGO_ENABLED=0` no longer works.

**System packages (Ubuntu/Debian):**

```bash
sudo apt-get install -y gcc g++ nlohmann-json3-dev
```

`nlohmann-json3-dev` provides `nlohmann/json_fwd.hpp`, which llama.cpp's v0.17.5 C++ tools require.

**Missing headers in the Go module cache:**

The ollama module at v0.17.5 includes multimodal C++ code (`mtmd`) that depends on two single-file libraries not bundled with the module. Download them manually:

```bash
OLLAMA_MOD="$(go env GOPATH)/pkg/mod/github.com/ollama/ollama@v0.17.5"
MTMD="$OLLAMA_MOD/llama/llama.cpp/tools/mtmd"

# Make module cache writable (read-only by default)
chmod -R u+w "$OLLAMA_MOD/llama"

# miniaudio — audio I/O library
mkdir -p "$MTMD/miniaudio"
curl -fsSL https://raw.githubusercontent.com/mackron/miniaudio/master/miniaudio.h \
     -o "$MTMD/miniaudio/miniaudio.h"

# stb_image — image loading library
mkdir -p "$MTMD/stb"
curl -fsSL https://raw.githubusercontent.com/nothings/stb/master/stb_image.h \
     -o "$MTMD/stb/stb_image.h"
```

These steps are one-time per machine. After that, `make build-cli` (which sets `CGO_ENABLED=1`) will compile cleanly. To verify the CGo layer alone:

```bash
CGO_ENABLED=1 go build ./internal/modelrepo/local/...
```

**Why these headers aren't in the module:** Go module distributions don't include all C/C++ source trees needed by every possible build tag. v0.17.5 added multimodal support whose C++ files pull in miniaudio and stb_image as relative includes. Until Ollama bundles them, the manual download is the workaround.

### Running Beam (HTTP server + embedded UI)

The binary serves the **built** React app embedded from `internal/web/beam/dist`. After UI changes, run **`make build-web`** (or `npm run build` in the workspace), then rebuild the Go binary so `//go:embed` picks up assets.

```bash
make build-cli
make build-web    # if you changed packages/beam or packages/ui
make build-cli
./bin/contenox beam   # default :8081 — regenerate OpenAPI after HTTP/route changes: make docs-gen
```

**Beam sign-in** (`http://127.0.0.1:8081/login`): default username **`admin`**, password **`admin`** (matches `internal/auth/simple.go`).

### Beam UI: full-stack development (recommended for frontend)

Use **two terminals** so the browser gets Vite HMR while talking to a real API:

1. **Terminal 1 — Go live reload:** `make deps-go-watch` once, then **`make dev-go-watch`** (runs `contenox beam` on `:8081` and rebuilds on Go changes).
2. **Terminal 2 — Vite with API proxy:** **`make dev-web-proxy`** (uses `packages/beam/.env.proxy`; proxies `/api` to the backend). Start (1) before (2), or Vite logs `ECONNREFUSED 127.0.0.1:8081` until the API is listening.

Open the **Vite dev server URL** in the browser (e.g. `http://localhost:5173`), not `http://localhost:8081`, so hot reload and same-origin `/api` work.

**`make dev-web`** (Vite without proxy) does not register the `/api` → `:8081` proxy. For API access from the Beam UI in that mode, set `VITE_API_BASE_URL=http://127.0.0.1:8081` in `packages/beam/.env.local`, or use **`make dev-web-proxy`** (recommended for full-stack).

Optional: **`make dev-cli`** builds and symlinks `contenox` to `~/.local/bin/contenox` for a system-wide command during development.

### Enterprise marketing site (`enterprise/site`)

The Next.js site includes documentation under `/docs`. **Cross-page doc search** loads a static index built from `enterprise/site/content/docs/**/*.md`. The generated file `enterprise/site/public/docs-search-index.json` is **gitignored**; it is produced automatically before `npm run dev` and `npm run build` (`prebuild`). If you change markdown under `content/docs/` and need the index without a full restart, run **`npm run docs:search-index`** in `enterprise/site`.

## Running tests

Before submitting a pull request, ensure tests pass.

**Fast path (matches CI):** tests whose names start with `TestUnit_`, with `-short` and a 15-minute cap:

```bash
make test-unit
```

**Full Go suite:** includes `TestSystem_*` integration tests (Docker, Ollama, vLLM containers, etc.) — slower and needs those tools:

```bash
make test              # same as: go test ./...
make test-system       # only TestSystem_*
```

**CLI package (verbose):**

```bash
make test-cli-verbose
```

**CLI help drift check** (after changing Cobra commands or flags): build first, then:

```bash
make test-cli-help
```

**Race detector (optional):**

```bash
go test -race ./... -run '^TestUnit_'
```

**HTTP API tests** (pytest): **`make test-http-api-venv`** prepares the venv; **`make test-http-api`** builds the CLI, starts a temporary `contenox beam`, and runs pytest. Set **`TEST_FILE`** to the test module under `apitests/` (see `Makefile` for the exact pytest invocation).

**OpenAPI → TypeScript client smoke test:**

```bash
make test-openapi-client-codegen
```

(or the npm script that wraps the same script — see `package.json`).

**Lint / format** the Beam app: **`make lint-web`**, **`make format-web`**.

## Pull request guidelines

1. **Create an issue:** If you're adding a major feature or changing the architecture, please open an issue first to discuss the design.
2. **Branch naming:** Create a branch from `main` with a descriptive name (`feature/xyz`, `fix/abc`, `docs/def`).
3. **Commit messages:** Follow [Conventional Commits](https://www.conventionalcommits.org/). For example, `feat: add support for local mode`, `fix: correct token count logic`, `docs: clarify CLI usage`.
4. **Style checks:** Ensure your code runs successfully through `gofmt` and standard linters like `golangci-lint` if available.
5. **No breaking changes:** Avoid breaking existing workflows or changing the API schema unexpectedly. Keep the HTTP API backward compatible when you change routes or DTOs.

## Code conventions

### Go doc comments

- Start exported declaration comments with the symbol name and a present-tense verb (`// Service manages …`, `// ParseEnv builds …`).
- Prefer one sentence. Add more lines only when a caller cannot use the API correctly without them.
- No narrative, change history, or first-person ("we used to", "I added"). The git log is the changelog.
- Reference other symbols with `[Pkg.Name]` so godoc can link them.
- Unexported helpers usually need no doc comment; inline `// ...` only when the logic is non-obvious.

### API-spec annotations

`make docs-gen` extracts the OpenAPI spec from Go types using inline markers on the lines that decode requests and encode responses. Two forms are required on every HTTP handler:

```go
req, err := apiframework.Decode[createSessionRequest](r) // @request terminalapi.createSessionRequest
...
_ = apiframework.Encode(w, r, http.StatusCreated, resp)  // @response terminalapi.createSessionResponse
```

- The marker must sit on the same line as the `Decode` / `Encode` call.
- Use the fully qualified Go type name (`pkg.Type`, `*pkg.Type`, or `[]*pkg.Type`).
- Path and query parameters are picked up from `apiframework.GetPathParam` / `GetQueryParam` — pass a human-readable description as the last argument so it lands in the spec.
- After touching a handler signature, request type, response type, or marker, run `make docs-gen` and check the diff under `docs/openapi.*` is what you intended (it is gitignored, so it will not be committed).

## Generating API documentation

The OpenAPI spec (`docs/openapi.json` and `docs/openapi.yaml`) is generated from the Go types. `make docs-gen` also copies the JSON into `internal/openapidocs/openapi.json` so it can be **embedded in the `contenox` binary**.

With **`contenox beam`** running (default `http://127.0.0.1:8081`), the same spec is served without the JWT stack, similar to FastAPI’s pattern:

- **`GET /openapi.json`** — raw OpenAPI 3.1 JSON  
- **`GET /docs`** — RapiDoc UI (loads the embedded spec from `/openapi.json`)

If you modify server endpoints or shapes:

```bash
make docs-gen         # JSON + YAML + embedded copy for builds
make docs-html        # standalone RapiDoc HTML under docs/ (depends on docs-gen)
make docs-markdown    # optional: large api-reference.md (Docker or npx)
```

Do **not** commit generated OpenAPI/embed outputs (`docs/openapi.*`, `internal/openapidocs/openapi.json`, `internal/web/beam/dist/`). Those paths are gitignored where applicable. **`make docs-gen`** drops a tiny stub JSON if needed, runs codegen, then copies the real spec into `internal/openapidocs/` for `//go:embed`. **`make build-cli`**, **`make test`**, **`make test-unit`**, and related test targets run **`docs-gen` first** so you do not commit or hand-maintain that file.

GitHub Actions runs **`make ci-prepare-embeds`** (Beam UI + stub) then **`make docs-gen`** before **`make build-cli`**.
