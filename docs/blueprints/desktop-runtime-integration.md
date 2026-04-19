# Blueprint: Desktop Runtime Integration

**Status:** Design / roadmap (not implemented yet).

---

## Problem

The desktop app (`contenox`) is a single binary: no arguments opens Beam, any arguments run the CLI. The Electron layer currently does nothing beyond starting the Go server and opening a window. That is a missed opportunity — Electron has access to OS-level capabilities the Go runtime cannot use directly.

---

## Goal

Make the Electron layer a genuine integration surface, not a pass-through wrapper. The terminal stays the execution context. Beam becomes the review and approval surface. The two sides communicate through the Electron process as the bridge.

---

## Architecture

```
terminal (user)
     │
     ▼
contenox <args>          ← Electron main process
     │
     ├─ no args ──────── open Beam window
     │
     └─ with args ────── exec Go binary, pipe stdio
                              │
                              └─ Go binary ──── HTTP server (Beam backend)
```

The Electron layer sits between the terminal and the Go runtime. This position gives it information neither side has alone: whether the window is open, OS focus state, and the ability to raise native OS surfaces.

---

## Capabilities unlocked

### Native HITL

Today, human-in-the-loop pauses execution in the terminal and waits for a keypress. With the desktop layer:

1. CLI hits a HITL step.
2. Electron receives the pause event from the Go runtime.
3. OS notification fires: "Contenox needs your review — plan step N."
4. Clicking the notification focuses the Beam window and navigates to the step.
5. User approves or edits in Beam.
6. Execution resumes in the terminal.

The terminal stays alive. Beam is the review surface. No terminal prompt required for approval.

### Protocol handler (`contenox://`)

Register `contenox://` as an OS protocol. Deep-links from the browser, notifications, or external tools can open Beam at a specific plan, session, or step without the user finding the window manually.

### OS notifications for long-running plans

`--auto` runs can notify on completion, failure, or when HITL is required — even when the window is minimised or the terminal is not in focus.

---

## Implications

- **No integrated terminal needed in Beam.** The terminal is already open — that is where the CLI runs. Beam does not need to replicate it. This keeps Beam focused on its role: canvas, review, and state.
- **CLI output and Beam state stay in sync** because the Go runtime is the single source of truth for both.

---

## Implementation Phases

**Phase 1 — IPC channel between Electron and Go runtime**
- Go runtime emits structured events (HITL pause, step complete, error) over a local socket or stderr stream.
- Electron main process listens and routes events to the window or OS layer.

**Phase 2 — Native HITL notifications**
- Electron fires OS notification on HITL pause.
- Notification action focuses window and navigates to the relevant plan step.
- Approval/rejection in Beam unblocks the CLI.

**Phase 3 — Protocol handler**
- Register `contenox://` OS handler.
- Support deep-links to plans, sessions, and steps.

---

## Open Questions

1. IPC transport: stderr structured JSON, Unix socket, or named pipe? Cross-platform considerations for the socket path.
2. Should notification actions (approve/reject) work without opening the full window, or is window focus always required?
3. How does this interact with headless / server deployments where no Electron layer is present?
