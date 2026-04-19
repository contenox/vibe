# Blueprint: Beam Remote Connector

## Problem

The founder's workflow: run the Contenox engine on a remote prod server, control it from a laptop. Today this means manually forwarding ports, managing SSH tunnels, and keeping browser tabs alive. There's no first-class remote story.

## Goal

A tiny headless binary (`contenox-connector`) that runs on the remote machine and exposes the full Contenox engine over a secure connection. Beam Desktop (or a browser) connects to it as if it were a local workspace.

**Target UX:**

```bash
# On remote server:
contenox-connector --host 0.0.0.0 --ssh-key /root/.ssh/id_ed25519

# On laptop (Beam Desktop):
# "Connect Remote" → paste host + key (or Tailscale hostname)
# → remote appears in workspace list as "prod-server (remote)"
# → all actions (chat, chains, HITL, files, terminal) work exactly as local
```

One tiny daemon. No open ports unless you choose. No sidecar, no VPN required (Tailscale optional).

---

## Architecture

**Beam Connector** = same Go codebase, built headless with a build tag.

```bash
go build -tags=connector -o contenox-connector ./cmd/connector
```

The connector binary strips the Wails/desktop shell and exposes only the engine API surface over a secure transport.

### Transport options

| Option | Use case | Setup |
|---|---|---|
| SSH tunnel (default) | Direct server access with key | `--ssh-key` flag; no open ports |
| Tailscale | Zero-config mesh networking | Just use the Tailscale hostname |
| Cloudflare Tunnel | Public-facing / no static IP | `cloudflared` sidecar, token in env |

### Connection protocol

- Beam Desktop initiates the connection (client)
- Connector exposes a minimal HTTP/WebSocket API (same routes as `contenox beam`)
- SSH key auth is the default; mTLS and bearer token are alternatives (open question)
- All HITL approval events are forwarded back to the controlling Beam Desktop instance

### What the connector exposes

Same API surface as `contenox beam` — no new routes required. The desktop client connects to it as a remote workspace endpoint, identical to how it connects to a local engine.

---

## UX in Beam Desktop

- "Connect Remote" modal: Host (or Tailscale name) + SSH key path (or key content) + optional port
- Remote workspace appears in the workspace switcher with a `(remote)` badge
- Latency indicator visible in workspace header
- Disconnect/reconnect without losing session state (sessions survive on the remote side)

---

## Implementation Phases

**Phase 1 – Headless Build (1 week)**
- Build tag to strip Wails/desktop dependencies
- `cmd/connector/main.go` entry point — starts engine + HTTP server, no UI
- Smoke test: `contenox-connector` on Linux server, `contenox beam` on laptop pointing at it

**Phase 2 – Secure Transport (1 week)**
- SSH key auth implementation
- Connection handshake + session token
- Tailscale path (zero extra work — just a hostname)

**Phase 3 – Desktop Integration (1 week)**
- "Connect Remote" flow in Beam Desktop UI
- Remote workspace in workspace switcher
- HITL approval forwarding to desktop

---

## Open Questions

1. Authentication: SSH key only for MVP, or also mTLS / bearer token from day one?
2. Should the connector support multiple simultaneous Beam Desktop clients (team use)?
3. Should connector state (sessions, chains) sync back to local on disconnect, or stay remote-only?
4. Packaging: distribute as a static Linux binary via GitHub Releases, or also as a `systemd` service unit?

---

## Relationship to Beam Desktop

The Remote Connector is a separate roadmap item from Beam Desktop but depends on it for the full UX — the "Connect Remote" flow lives in the desktop app. The connector itself (`contenox-connector`) is usable standalone: any HTTP client (including a plain browser pointing at a forwarded port) can talk to it.

**Sequencing:** Beam Desktop Phase 0–1 first, then Remote Connector Phase 1–2 in parallel with Beam Desktop Phase 2.
