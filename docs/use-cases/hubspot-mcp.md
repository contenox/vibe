---
description: Read and write your HubSpot CRM through HubSpot's own MCP server — OAuth 2.1 + PKCE, tokens stored locally.
---

# HubSpot via MCP

Read and write your HubSpot CRM with contenox using HubSpot's own MCP server — OAuth 2.1 + PKCE, your tokens stored locally, your data routed direct between CLI and HubSpot.

This is the MCP route: HubSpot's full curated tool set, OAuth with pre-issued client credentials. For a narrower, hand-curated tool surface instead, register HubSpot's REST API yourself as a [remote tool](/docs/integrations/tools/remote/) — see [Any API, a tool you authored](/docs/use-cases/any-api-as-a-tool/) for the pattern.

The wider point: the **OAuth-with-pre-issued-credentials** path also works for Salesforce, Microsoft Graph, and any other vendor MCP that requires a manually-registered OAuth app (no RFC 7591 dynamic client registration). HubSpot is just the example.

---

## Prerequisites

- contenox **v0.35.0+** *(the release containing `--oauth-client-id` / `--oauth-client-secret-env` support on `mcp add`)*
- A configured LLM backend with tool calling
- A HubSpot CRM portal you have admin access to
- A HubSpot developer account (free, same login as your CRM)

---

## 1. Create an MCP Auth App in HubSpot

In `developers.hubspot.com`:

1. Sidebar → **Development** → **MCP Auth Apps**
2. Top-right → **Create MCP auth app**
3. Fill in:
   - **Name:** anything readable, e.g. `contenox-local` (shows up on the OAuth consent screen)
   - **Description:** optional
   - **Weiterleitungs-URL / Redirect URL:** `http://127.0.0.1:49152/callback` *(this is contenox's default callback; the port must match exactly — see Caveats below for changing it)*
   - **Symbol:** skip — only required for marketplace certification
4. Click **Create**

You'll be redirected to the app's details page. Copy two values:

- **Client ID** (UUID)
- **Client Secret** (separate UUID — click "Show" or "Reveal" if it's hidden behind a button)

---

## 2. Register the MCP server with Contenox

```bash
export HUBSPOT_MCP_CLIENT_SECRET=<the client_secret from HubSpot, NOT the client_id>

contenox mcp add hubspot \
    --transport http \
    --url https://mcp.hubspot.com/ \
    --auth-type oauth \
    --oauth-client-id <client_id from HubSpot> \
    --oauth-client-secret-env HUBSPOT_MCP_CLIENT_SECRET
```

What `--oauth-client-secret-env` does: contenox stores only the env var **name** in its local SQLite, not the secret value. At each connection, the secret is resolved from your environment at runtime.

---

## 3. Authorize in the browser

```bash
contenox mcp auth hubspot
```

This opens your browser at HubSpot's authorization URL. You:

1. Pick which HubSpot portal to authorize
2. Review the scopes (HubSpot determines them automatically from the MCP server's current tool set and your user permissions)
3. Click **Approve**

contenox catches the redirect at `http://127.0.0.1:49152/callback`, exchanges the code for an access token + refresh token using your client_secret, and persists the tokens locally. From now on, refresh is automatic until the refresh token expires.

Real output from a fresh run:

```text
Opening browser for contenox-local authorization...
hubspot: authenticated successfully.
```

---

## 4. Use it

Once attached, HubSpot's tools are available like any other MCP server's — to an agent session with this server in scope, or to a `contenox serve` deployment with it registered. Ask about your CRM and the model reaches for `search_crm_objects`, `get_crm_objects`, and friends as needed, subject to your active HITL policy.

---

## What HubSpot's MCP exposes

Per HubSpot's [official documentation](https://developers.hubspot.com/docs/apps/developer-platform/build-apps/integrate-with-the-remote-hubspot-mcp-server), the tools the model gets to see:

- `search_crm_objects` — search CRM records with filters and pagination
- `get_crm_objects` — fetch up to 100 records by ID
- `manage_crm_objects` — create or update records and activities
- `search_properties` / `get_properties` — discover schema
- `search_owners` — look up CRM record owners
- `get_user_details` — authenticated user info
- `get_campaign_contacts_by_type`, `get_campaign_analytics`, `get_campaign_asset_types`, `get_campaign_asset_metrics` — campaign analytics

The supported objects: contacts, companies, deals, tickets, line items, products (write); plus calls, emails, meetings, notes, tasks (activities, write); plus quotes, subscriptions, segments, blog posts, landing pages, site pages, campaigns, marketing events (read).

---

## Customize

- **Callback port is fixed.** The OAuth redirect listener always binds `127.0.0.1:49152` — there is no config key to change it. Make sure the port is free before starting the flow, and register `http://127.0.0.1:49152/callback` exactly in HubSpot's MCP Auth App.
- **Different OAuth-only MCP.** Same flags work for any vendor whose MCP requires a manually-registered OAuth app (Salesforce, Microsoft Graph). Create the app in their UI, register the redirect URL `http://127.0.0.1:49152/callback`, then `contenox mcp add <name> --auth-type oauth --oauth-client-id ... --oauth-client-secret-env ...`.

---

## Caveats

- **Scopes are determined by HubSpot, not you.** Per their docs: "available scopes are automatically determined by (1) the tools available in the MCP server at the time of installation and (2) the permissions that the user chooses to grant during installation." You can't pre-declare scopes; the user picks at consent time.
- **Sensitive Data setting blocks activities.** If your HubSpot account has Sensitive Data turned on, the MCP server blocks access to activity objects (calls, emails, meetings, notes, tasks) — even though they're listed as supported. This is HubSpot-specific behavior; standard CRM API calls are unaffected.
- **Token refresh on stale sessions.** If the refresh token expires (long inactivity, or you revoked access in HubSpot), `contenox mcp auth hubspot` re-runs the browser flow cleanly.
- **The MCP server uses HubSpot's CRM search API under the hood**, which doesn't include vector search. For semantic similarity over CRM records, you'd still need a separate embedding pipeline.
- **A hand-curated tool surface still has its place.** For workflows where you want "the agent can only create companies and contacts, nothing else," a [registered remote tool](/docs/use-cases/any-api-as-a-tool/) against a narrow spec is the better fit — `manage_crm_objects` in the MCP is broad enough that scoping it down requires HITL policy rules, not spec subsetting.
