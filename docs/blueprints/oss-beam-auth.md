# OSS Beam auth — single operator, default `admin` / `admin`, changeable password

**Status:** Design / roadmap (not implemented yet).  
**Intent:** Good first issue for contributors who want a bounded, user-visible feature on the open-source `contenox beam` stack.

---

## Goal

The OSS runtime (`contenox beam`) targets a **single local operator**: one account, no multi-tenant user directory. Ship with:

- **Default credentials:** username **`admin`**, password **`admin`** (documented in the quickstart and `CONTRIBUTING.md`).
- **Later:** let that operator **change the password** from Beam (or CLI), persisted locally, without pulling in the enterprise Postgres user system.

Enterprise Contenox (`enterprise/bob`, full access control, hashed users in Postgres, etc.) remains separate; this blueprint is **only** for the OSS/local path wired through `internal/auth/simple.go` and `internal/server/server.go`.

---

## Current implementation (what exists today)

| Piece | Location | Behavior |
|-------|----------|----------|
| Login + JWT | `internal/auth/simple.go` | `SimpleTokenManager`: fixed `admin` / `admin`, fixed JWT secret in source, 24h TTL. |
| HTTP routes | `internal/authapi/authroutes.go` | `POST /login`, `POST /ui/login`, cookie-based UI flow, etc. |
| Server wiring | `internal/server/server.go` | Constructs `auth.NewSimpleTokenManager(...)` and registers auth routes. |
| Beam UI | `packages/beam` | Login form posts to `/api/ui/login`; first field is “username” (JSON field name `email` for history). |

**Non-goals for the first iteration:** multiple users, roles, email verification, OAuth, or parity with `enterprise/bob/userservice`.

---

## Design direction (password change)

### 1. Persistence

Store **one** bcrypt (or Argon2) hash for the operator password in the **existing SQLite** database used by Contenox Local (same `local.db` / `runtimetypes` schema world as the rest of OSS), not in the repo.

Options (pick one in implementation):

- **A — Small dedicated table** in `runtimetypes/schema_sqlite.sql`, e.g. `local_auth_credentials` with a single row (`password_hash`, `updated_at`), migrated on startup; or  
- **B — KV key** via `runtimetypes.Store` / the same pattern as CLI config (`internal/clikv`), e.g. key `auth.admin_password_bcrypt`.

**Bootstrap rule:** If no hash is stored, **`admin` / `admin`** remains valid (first-run experience). After the operator sets a password, login checks only the stored hash (plus optional “force reset” story later).

### 2. `SimpleTokenManager` evolution

- **Login:** Compare against stored hash when present; otherwise fall back to default `admin` / `admin`.
- **Change password:** New method used only after session auth, e.g. `ChangePassword(ctx, current, new string) error` with constant-time comparison for current password.
- **JWT signing key:** Stop hardcoding `your-secret-key-change-this` for production-minded installs; read from env (e.g. `CONTENOX_JWT_SECRET`) or a file under `.contenox/` — can be a **follow-up** issue if the first PR only does password storage + change.

### 3. HTTP API

- **`POST /api/.../change-password`** (exact path to match existing auth style): body `current_password`, `new_password`; **requires** valid JWT/cookie (same middleware stack as other authenticated routes).
- OpenAPI: regenerate with `make docs-gen` after adding handlers.

### 4. Beam UI

- New screen or section under admin/settings: **“Security”** or **“Account”** — form: current password, new password, confirm. Calls the new endpoint; show success/error.

### 5. Documentation

- Update quickstart / README when the feature ships: default remains `admin` / `admin` until changed; link to how to change password.
- Optional: `contenox` CLI command to set or reset password for headless setups.

---

## Suggested scope for a “good first issue”

**In scope (single PR is realistic if scoped tightly):**

1. Schema or KV key + migration/init for one password hash.  
2. `SimpleTokenManager` (or a thin wrapper) reads/writes hash; login behavior + `ChangePassword`.  
3. One authenticated HTTP route + tests (`TestUnit_*`).  
4. Minimal Beam UI form + i18n keys.

**Out of scope (separate issues):**

- JWT secret rotation / env-only secret  
- CLI `contenox auth reset-password`  
- Recovery if password is lost (without reinstalling DB)  
- Anything under `enterprise/bob`

---

## Acceptance criteria (for the implementing PR)

- [ ] Fresh install: login with **`admin` / `admin`** works.  
- [ ] After change-password: old password rejected, new password works; session handling still correct.  
- [ ] No plaintext password stored; hash uses a standard library (e.g. `golang.org/x/crypto/bcrypt`).  
- [ ] Docs updated (quickstart + `CONTRIBUTING` snippet) describing default + optional change.  
- [ ] `make test-unit` passes; new tests cover auth logic without requiring Beam E2E.

---

## References

- `internal/auth/simple.go` — current hardcoded credentials and JWT.  
- `internal/authapi/authroutes.go` — login routes.  
- `internal/server/server.go` — auth wiring.  
- `enterprise/bob/...` — **not** the template for OSS; only reference for “how we hash passwords elsewhere” if useful.  
- `docs/blueprints/local-mode-spec.md` — context on SQLite/local mode.

---

## GitHub issue title (suggested)

**`feat(beam): persist OSS admin password + change-password UI`**

**Labels:** `good first issue`, `enhancement`, `beam` (or your project equivalents).

**Body:** Paste the **Goal**, **Suggested scope**, and **Acceptance criteria** sections above; link to this file for full detail.
