# Blueprint: Beam Desktop — Wails Single-Binary Desktop Application

## Problem

`contenox beam` runs an HTTP server you open in a browser tab. Every workspace is a new server on a new port. State is global, not per-project. There's no native window, no tray, no first-class multi-workspace experience.

## Goal

A single double-clickable binary that opens Beam as a native desktop window — same Go engine, same React UI, zero architecture changes.

**Core problems solved:**
- One workspace = one app window (no port juggling, no disconnected browser tabs)
- All state (chats, credentials, chains, HITL policies) scoped per folder instead of globally
- `contenox beam` CLI continues to work unchanged for scripts and power users

**Target UX:**

```bash
contenox init        # scaffolds .contenox/ in current folder
contenox             # double-click → Beam Desktop opens as native window
```

---

## Why Wails

| Criteria | Wails (Go) | Tauri 2 (Rust) | Electron + Go | Pure Native (Fyne/Gio) |
|---|---|---|---|---|
| Single clean binary | Yes | Yes (sidecar) | Hacky | Yes |
| 100% reuse existing Go API | Yes | Yes (sidecar) | Yes | Yes |
| 100% reuse React UI | Yes (`//go:embed`) | Yes | Yes | No (rewrite) |
| Multi-workspace + global state | Yes | Yes | Yes | Yes |
| Developer velocity (Go team) | Highest | Medium | High | Low |
| Binary size | ~15–40 MB | Very small | ~120 MB+ | Very small |

Wails: zero architecture flip, full reuse of existing Go + React, single binary purity.

---

## Architecture

**Beam Desktop** = existing Go engine + Wails shell, React UI embedded via `//go:embed`.

### New entry point

```go
// cmd/desktop/main.go
//go:embed frontend/dist
var frontend embed.FS

func main() {
    app := wails.NewApp(&wails.AppConfig{
        Title:            "Contenox Beam",
        Width:            1440,
        Height:           900,
        BackgroundColour: &wails.RGBA{R: 0, G: 0, B: 0, A: 255},
        Assets:           frontend,
    })
    app.Bind(existingServices...)
    app.Run()
}
```

### React integration (zero rewrite)

- `npm run build` → `frontend/dist` → embedded in binary
- Dev: Wails proxies to existing Vite dev server (`http://localhost:5173`) for hot reload
- Prod: `wails build` → one binary

### New Go layer (minimal)

- Workspace management service — attach/detach roots, global vs per-workspace config
- Global config store (`~/.contenox/config.json` + per-workspace overrides)
- Persistent session store (chats, agent memory, execution state)

---

## UX Spec

- **Left sidebar**: Workspace switcher (attached folders + "+ Add workspace")
- **Global header bar**: Chain selector, Mode selector, HITL policy quick-switch
- **Main area**: Existing React chat canvas + files + terminal tabs (unchanged)
- **Right panel** (collapsible): Global credentials, favourite chains, recent sessions
- **Tray icon** (macOS / Windows / Linux): Quick "New workspace", "Connect remote", "Quit"

### Key flows

1. **First launch** — `contenox` → native window opens → prompts to attach first workspace.
2. **Multi-workspace** — attach 2–N folders; switching updates files/terminal context while keeping chat history and global policies alive.
3. **Global vs per-workspace state** — credentials, HITL policies, favourite chains live globally; overridable per workspace.
4. **CLI parity** — `contenox beam` continues to start a headless HTTP server unchanged.

---

## Implementation Phases

**Phase 0 – Proof of Concept (1 week)**
- Wails project skeleton + embed current React build
- Basic window with workspace switcher stub
- Deliverable: clickable single binary that opens Beam

**Phase 1 – Core Desktop (2–3 weeks)**
- Workspace attach/detach in Go
- Global config + persistent session store
- React sidebar for workspace switcher + global settings
- Tray icon + native menus

**Phase 2 – Polish & Release (1–2 weeks)**
- Native window controls, drag title bar, dark mode parity
- Migration path from old `contenox beam` per-workspace servers
- Packaging: `.deb`, `.rpm`, Homebrew, Windows installer

---

## Open Questions

1. Should the desktop binary also support fully headless mode (for CI), or keep the CLI separate?
2. Should global HITL policies and credentials be encrypted at rest via OS keychain (Wails supports this)?
3. Long-term: can Beam Desktop become a lightweight MCP server so other editors can connect to it?

---

## References
- Wails v2: https://wails.io
- Current Contenox Go + React architecture (internal)
