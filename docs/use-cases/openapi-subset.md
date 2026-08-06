---
title: Authoring your tool inventory
description: Expose a curated subset of an OpenAPI spec as agent tools — least privilege at registration time.
---

# Authoring your tool inventory

Cut a large OpenAPI spec down to a hand-curated subset and register only that, so the assistant sees a handful of clear operations instead of an entire API.

Handing an agent a 200-endpoint spec exhausts its context window with endpoints it will never use, gives the planner multiple ways to do the same thing, and widens the blast radius if the model hallucinates a call. Registering a narrow subset avoids all three without writing any integration code.

## Prerequisites

- `contenox` installed.
- The target API's OpenAPI spec (`openapi.json`/`.yaml`), or the ability to write one by hand.

## Steps

1. Pull the full spec (`GET <url>/openapi.json`, or the vendor's published spec).

2. Delete every path and operation the assistant shouldn't have. Keep only what a specific task needs — e.g. `GET /invoices/{id}` and `POST /support/escalate` — and save the result as `support-subset.yaml`.

3. Register it, pointing traffic at the real API and the spec at your curated file:

   ```bash
   contenox tools add erp_support \
     --url https://erp.internal.example.com \
     --spec ./specs/support-subset.yaml \
     --inject "tenant_id=acme"
   ```

   The model sees a tool named `erp_support` with exactly the operations in the subset — never the other endpoints, and never the injected `tenant_id`.

4. To split one monolithic API into several narrow tools, register it multiple times under different names, each with its own subset spec:

   ```bash
   contenox tools add erp_billing   --url https://erp.internal.example.com --spec ./specs/billing.yaml
   contenox tools add erp_vacation  --url https://erp.internal.example.com --spec ./specs/hr.yaml
   contenox tools add erp_inventory --url https://erp.internal.example.com --spec ./specs/warehouse.yaml
   ```

5. Reference only the tool(s) a chain needs in its `execute_config.tools` allowlist:

   ```json
   {
     "execute_config": {
       "tools": ["erp_vacation", "local_shell"]
     }
   }
   ```

## Expected outcome

A chain given `erp_vacation` can call only the operations in `hr.yaml` — nothing from `billing.yaml` or `warehouse.yaml`, and nothing outside the curated subset even though all three point at the same underlying API.

## Customize

The spec path (`--spec`) is independent of where traffic goes (`--url`), so adding a capability later is a YAML edit and a `contenox tools update --spec <file>`, not a new integration.

## Where to next

- [Any API, a tool you authored](/docs/use-cases/any-api-as-a-tool/) — hiding credentials and injected parameters on top of a curated spec.
- [The nested permission bomb](/docs/use-cases/nested-permission-bomb/) — why a workflow shouldn't get more surface than it needs.
- [Remote tools](/docs/integrations/tools/remote/) — the `tools add`/`update`/`show` reference in full.
