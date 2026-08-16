---
title: Any API, a tool you authored
description: Contenox turns any HTTP API into a scoped, credential-hidden, policy-governed tool — so an assistant, even an untrusted one, reaches exactly the slice you authored and nothing more.
---

# Any API, a tool you authored

Register an HTTP API as a tool with the credential hidden from the model, the endpoint surface narrowed to a hand-curated subset, and — for untrusted drivers — an explicit approval rule.

## Prerequisites

- `contenox` installed and a backend configured with a tool-calling model — see [Quickstart](/docs/guide/quickstart/).
- An HTTP API to expose, a credential for it, and (optionally) a hand-curated OpenAPI subset spec.

## Steps

1. Curate an OpenAPI subset spec listing only the operations the assistant needs. Skip this step to register the vendor's full spec instead — see [Authoring your tool inventory](/docs/use-cases/openapi-subset/).

2. Register the API as a tool, injecting the credential and any fixed parameters so the model never sees them:

   ```bash
   contenox tools add crm \
     --url https://api.vendor.com \
     --header "Authorization: Bearer $CRM_TOKEN" \
     --inject "caller_id=assistant-01" \
     --spec ./crm-readonly.json
   ```

   `--header` and `--inject` are bound server-side — the model can't read or tamper with the token or the injected parameter. `--spec` limits what the assistant can call to exactly the operations in that file, regardless of what the vendor's full API exposes.

3. Reference the tool from a chain's `execute_config.tools` allowlist, or rely on `"tools": ["*"]` in the default chain so it's picked up automatically.

4. For an interactive, device-owner session (`contenox acp` — Zed, JetBrains — or `contenox serve`), calls route through HITL: allow, deny, or approve per call, per your active policy.

5. For an untrusted, non-interactive driver (`contenox acpx` — see [Use from OpenClaw](/docs/integrations/editors/openclaw/)), add an explicit `allow` rule for the tool to `hitl-policy-acpx.json`:

   ```json
   { "tools": "crm", "tool": "read_contacts", "action": "allow" }
   ```

   `hitl-policy-acpx.json` defaults to `default_action: deny`. Registering a tool does not make it callable under `acpx` — until a rule allows it explicitly, the assistant is refused with no prompt and no error.

## Expected outcome

- A device-owner session can call the tool and, for any call your policy gates, is prompted to approve or deny it.
- An `acpx` session can call only the tools you explicitly allowed in its policy file — everything else is silently refused.

## Where to next

- [The nested permission bomb](/docs/use-cases/nested-permission-bomb/) — why inherited human access is the anti-pattern this replaces.
- [Use from OpenClaw](/docs/integrations/editors/openclaw/) — the untrusted-driver profile, wired end to end.
- [HITL policies](/docs/guide/hitl/) — the authored allow/deny file itself.
- [Remote tools](/docs/integrations/tools/remote/) — registering an API as a tool, in full.
