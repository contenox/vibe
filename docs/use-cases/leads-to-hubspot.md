---
description: Tavily finds fresh leads on the web; an OpenAPI sub-spec writes them straight into HubSpot — companies, contacts, and associations, no glue code.
---

# Leads → HubSpot

Two contenox flows chained: Tavily MCP finds fresh leads on the web, then a narrow OpenAPI sub-spec writes them into HubSpot — companies, contacts, descriptions, associations.

End state: 5 real B2B SaaS leads, 5 HubSpot companies, 8 contacts, ~30 seconds for the CRM write step. Your tokens stay on your machine, your LLM is whichever backend you've configured.

The recipe also doubles as a tour of two different contenox tool-integration patterns:

- **Part 1** uses Tavily's hosted MCP server (RFC 7591 dynamic OAuth — runtime auths in a browser, no manual app config)
- **Part 2** uses a hand-curated OpenAPI sub-spec against HubSpot's REST API (bearer token, no MCP needed)

---

## Prerequisites

- A configured LLM backend with tool calling. Recipe uses `gemini-flash-latest` — fast enough that tool loops complete in seconds rather than minutes.
- A free Tavily account (for the lead-generation step) — sign up at [tavily.com](https://www.tavily.com/)
- A HubSpot CRM portal you have admin access to (for the write step)
- `python3` on `PATH` (any modern version — used for the leads-file splitter)

---

# Part 1 — Find leads with Tavily

## 1.1 Register Tavily's MCP server

```bash
contenox mcp add tavily --transport http --url https://mcp.tavily.com/mcp/ --auth-type oauth
contenox mcp auth tavily
```

The `auth` command opens your browser to Tavily's authorization page. Approve, and the local CLI catches the redirect, exchanges the code, and persists the tokens. No client_id / client_secret to manage — Tavily's MCP supports RFC 7591 dynamic client registration, so contenox negotiates everything at runtime.

Real output from a fresh run:

```text
MCP server "tavily" added successfully.
Opening browser for contenox authorization...
tavily: authenticated successfully.
```

## 1.2 Generate leads.txt with one chat command

```bash
contenox session new lead-discovery
contenox chat --model gemini-flash-latest --provider gemini --timeout 5m \
  "Use tavily to find 5 recent news articles or press releases about B2B SaaS startups in London that just raised Seed funding. For each, extract the company name, the CEO/Founder's name, and a 1-sentence summary of what they do. Save the results to leads.txt as blank-line-separated blocks formatted exactly as: '1. Company Name: <name>\n   Founder/CEO: <name>[ and <name>]\n   Summary: <one sentence>'."
```

The workflow calls `tavily.tavily_search` (one or more queries), parses results, then calls `local_fs.write_file` to write `leads.txt`. The file write triggers contenox's default HITL policy — you'll get an `Approve? [y/N]` prompt showing the proposed content. Confirm, and you have your leads.

> **If the workflow gets stuck in search-verification loops with a thinking-class model**, switch to `gemini-flash-latest` (or any fast tool-calling model). Reasoning models tend to second-guess search results; flash models commit to action faster, which is what you want for batch tool work.

`leads.txt` ends up looking like this (real sample, names verified against actual seed announcements):

```text
1. Company Name: Acme Robotics
   Founder/CEO: Jordan Lee
   Summary: A cybersecurity SaaS platform that helps organizations continuously detect, assess, and manage third-party and supply-chain cyber risks in real-time.

2. Company Name: Northwind Traders
   Founder/CEO: Morgan Reyes and Alex Kim
   Summary: Provides an agent-first billing and payments infrastructure designed specifically to help AI-native and agent-driven businesses monetize their usage and outcomes.

3. Company Name: Globex (Globex Treasury)
   Founder/CEO: Taylor Brooks and Sam Patel
   Summary: An AI-powered finance automation platform that streamlines treasury management, accounts payable, payroll, and FX for modern B2B finance teams.
```

---

# Part 2 — Push leads into HubSpot

## 2.1 Get a HubSpot credential

You need a bearer token with these CRM scopes:

- `crm.objects.companies.read`
- `crm.objects.companies.write`
- `crm.objects.contacts.read`
- `crm.objects.contacts.write`

Two routes work today, both produce a `pat-*-...` token that authenticates as `Authorization: Bearer <token>`:

- **Private App** — in your CRM portal: gear icon → Integrations → Private Apps → "Create private app". GA, recommended.
- **Service Key** — `developer.hubspot.com` → Service Keys (Public Beta). Same auth shape, same scopes.

Once you have it:

```bash
export HUBSPOT_TOKEN=pat-na1-...
```

> HubSpot also ships an OAuth-only MCP server at `https://mcp.hubspot.com/` with curated CRM tools. If you'd rather use that path — particularly for read-heavy workflows where you want HubSpot's full tool surface — see the [HubSpot via MCP recipe](/docs/use-cases/hubspot-mcp/). The OpenAPI route below is the right pick when you want a hand-curated, narrow tool surface (3 operations vs HubSpot's ~12) and finer write control, and it's the same pattern that works for any vendor shipping an OpenAPI spec without an MCP server.

---

## 2.2 Drop the OpenAPI sub-spec

Save to `~/.contenox/hubspot-revops.yaml` — a hand-curated 3-operation subset of HubSpot's CRM v3 REST API. Small enough to keep the model focused; covers everything this recipe needs.

```yaml
openapi: 3.0.3
info:
  title: HubSpot RevOps (narrow)
  version: "1.0.0"
servers:
  - url: https://api.hubapi.com

paths:
  /crm/v3/objects/companies/search:
    post:
      operationId: searchCompany
      summary: Search HubSpot companies by name for deduplication.
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [filterGroups]
              properties:
                filterGroups:
                  type: array
                  items:
                    type: object
                    required: [filters]
                    properties:
                      filters:
                        type: array
                        items:
                          type: object
                          required: [propertyName, operator]
                          properties:
                            propertyName: { type: string }
                            operator:
                              type: string
                              enum: [EQ, NEQ, CONTAINS_TOKEN, NOT_CONTAINS_TOKEN, IN, NOT_IN, HAS_PROPERTY, NOT_HAS_PROPERTY]
                            value: { type: string }
                            values:
                              type: array
                              items: { type: string }
                properties:
                  type: array
                  items: { type: string }
                limit: { type: integer }
                query: { type: string }
      responses:
        "200":
          description: Search results.
          content:
            application/json:
              schema:
                type: object
                properties:
                  total: { type: integer }
                  results:
                    type: array
                    items: { $ref: "#/components/schemas/SimpleObject" }

  /crm/v3/objects/companies:
    post:
      operationId: createCompany
      summary: Create a new HubSpot company.
      description: |
        Common properties: name, domain, description, industry, city, country, numberofemployees.
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: "#/components/schemas/CreateInput" }
      responses:
        "201":
          description: Company created.
          content:
            application/json:
              schema: { $ref: "#/components/schemas/SimpleObject" }

  /crm/v3/objects/contacts:
    post:
      operationId: createContact
      summary: Create a HubSpot contact, optionally associated with a company.
      description: |
        To link the contact to a company at create time, set associations to
        [{"to":{"id":"<company_id>"},"types":[{"associationCategory":"HUBSPOT_DEFINED","associationTypeId":279}]}].
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: "#/components/schemas/CreateInput" }
      responses:
        "201":
          description: Contact created.
          content:
            application/json:
              schema: { $ref: "#/components/schemas/SimpleObject" }

components:
  schemas:
    CreateInput:
      type: object
      required: [properties]
      properties:
        properties:
          type: object
          additionalProperties: { type: string }
        associations:
          type: array
          items:
            type: object
            required: [to, types]
            properties:
              to:
                type: object
                required: [id]
                properties:
                  id: { type: string }
              types:
                type: array
                items:
                  type: object
                  required: [associationCategory, associationTypeId]
                  properties:
                    associationCategory:
                      type: string
                      enum: [HUBSPOT_DEFINED, INTEGRATOR_DEFINED, USER_DEFINED]
                    associationTypeId: { type: integer }
    SimpleObject:
      type: object
      required: [id, properties]
      properties:
        id: { type: string }
        properties:
          type: object
          additionalProperties: { type: string }
        createdAt: { type: string, format: date-time }
        updatedAt: { type: string, format: date-time }
```

---

## 2.3 Register the tool

```bash
contenox tools add hubspot \
    --url https://api.hubapi.com \
    --spec ~/.contenox/hubspot-revops.yaml \
    --header "Authorization: Bearer $HUBSPOT_TOKEN"
```

Verify the 3 operations are wired:

```bash
contenox tools show hubspot
```

You should see `searchCompany`, `createCompany`, and `createContact` listed.

---

## 2.4 Drop the runner script

Save to `~/leads-to-hubspot.sh` and `chmod +x` it:

```bash
#!/usr/bin/env bash
# leads-to-hubspot.sh — feed a structured leads file through Contenox into HubSpot,
# one lead per agent invocation so each gets a fresh round budget.

set -euo pipefail
: "${HUBSPOT_TOKEN:?set HUBSPOT_TOKEN before running}"

LEADS_FILE="${1:-leads.txt}"
MODEL="${MODEL:-gemini-flash-latest}"
PROVIDER="${PROVIDER:-gemini}"
TIMEOUT="${TIMEOUT:-3m}"

read -r -d '' PROMPT <<'EOF' || true
You are a RevOps assistant. Process EXACTLY ONE lead below using the hubspot tools provider.

1. Call hubspot.searchCompany once with filterGroups=[{filters:[{propertyName:"name", operator:"EQ", value:<the company name>}]}]. If results is non-empty, output "SKIP <company name>: duplicate" and stop.
2. Otherwise call hubspot.createCompany with properties.name=<the company name> and properties.description=<the summary>. Capture the returned id as company_id.
3. For each Founder/CEO (split on " and "), call hubspot.createContact with properties.firstname, properties.lastname, properties.jobtitle="Founder/CEO", and associations=[{"to":{"id":<company_id>},"types":[{"associationCategory":"HUBSPOT_DEFINED","associationTypeId":279}]}].

Do NOT run extra searches. Be terse.
Final line: "OK <company name> id=<company_id> contacts=<comma-separated ids>"

Lead:
EOF

python3 -c '
import sys, re
text = open(sys.argv[1]).read()
for block in re.split(r"\n\s*\n", text):
    block = block.strip()
    if block: sys.stdout.write(block + "\0")
' "$LEADS_FILE" |
while IFS= read -r -d '' lead; do
    name=$(printf '%s' "$lead" | sed -n 's/.*Company Name:[[:space:]]*\(.*\)/\1/p' | head -1)
    printf '▶ %s\n' "${name:-<unknown>}" >&2
    contenox session new "lead-$(date +%s%N | tail -c 9)" >/dev/null
    contenox chat --timeout "$TIMEOUT" --model "$MODEL" --provider "$PROVIDER" \
        "${PROMPT}${lead}" </dev/null 2>/dev/null | tail -3
    printf '\n' >&2
done
```

> The `</dev/null` on the `contenox chat` line is load-bearing. Without it the loop's NUL-separated input gets consumed by the chat process and only the first lead runs.

---

## 2.5 Run the script

```bash
./leads-to-hubspot.sh leads.txt
```

Real output from a live run against an empty HubSpot portal:

```text
▶ Acme Robotics
OK Acme Robotics id=100000000001 contacts=200000000001

▶ Northwind Traders
OK Northwind Traders id=100000000002 contacts=200000000002,200000000003

▶ Globex (Globex Treasury)
OK Globex (Globex Treasury) id=100000000003 contacts=200000000004,200000000005
```

`▶` lines go to stderr (status), `OK` lines to stdout (machine-parseable: company name, id, comma-separated contact ids). Wall-time for 5 leads with 8 founders total: ~30 seconds.

---

## 2.6 Verify in HubSpot

Hit the API directly to confirm what landed:

```bash
curl -sS -H "Authorization: Bearer $HUBSPOT_TOKEN" \
  "https://api.hubapi.com/crm/v3/objects/companies?limit=20&properties=name,description&sort=-createdate" \
  | python3 -m json.tool
```

The new companies appear at the top with descriptions matching the `Summary:` lines from `leads.txt`. Same pattern at `/crm/v3/objects/contacts` for the founders — `firstname`, `lastname`, `jobtitle="Founder/CEO"`, associated to their company.

---

## Customize

- **Different lead source.** Swap Tavily for any other search MCP (Perplexity Sonar, Exa, You.com) — the lead-discovery prompt is provider-agnostic, just reference the matching `<provider>.search` tool. Or skip Part 1 entirely and bring your own pre-built leads.txt from any source.
- **Add fields.** Edit the runner prompt to map additional fields from leads.txt into `properties` on `createCompany` (e.g. `domain`, `industry`, `city`). No spec change needed — `properties` is an open map.
- **Add a notes step.** Drop a `createNote` operation into the spec (path `/crm/v3/objects/notes`, same `CreateInput` shape; `associationTypeId` 190 for Note→Company, 202 for Note→Contact). Requires `crm.objects.notes.write` scope — present on Private Apps, not on Service Keys at time of writing.
- **Different LLM.** Set `MODEL=...` and `PROVIDER=...` before running. Any tool-calling model works.

---

## Caveats

- **Tavily free tier limits.** $5 in monthly credits, ~1k searches/mo at the standard rate. Plenty for occasional lead-discovery; check your usage if you run it in a loop.
- **HubSpot Service Keys are Public Beta** at time of writing. The scope picker doesn't expose `crm.objects.notes.*` — use a Private App if you need notes.
- **HITL on file writes.** Part 1's `local_fs.write_file` call to save `leads.txt` triggers contenox's default approval prompt. That's by design — you review what the agent extracted before it lands on disk. The HubSpot create operations in Part 2 flow through without prompts because `hubspot` isn't in the default HITL policy. To require approval per CRM write, add a rule to `~/.contenox/hitl-policy-default.json`:

  ```json
  {"tools": "hubspot", "tool": "createCompany", "action": "approve"}
  ```

- **One lead per chat invocation.** Each lead gets a fresh agent round budget. Batching all leads into one prompt also works for small batches but hits the chat chain's tool-call budget around lead 3 (the default chat chain caps its loops at 12 main / 8 recovery rounds; the one-shot `contenox run` chain caps at 10).
- **Token storage.** Your `HUBSPOT_TOKEN` ends up in contenox's local SQLite (`~/.contenox/local.db`) inside the `remote_tools` row's headers. Same machine, same posture as any locally-stored credential — rotate as you would any other token. Tavily OAuth tokens are stored the same way under the MCP server's row.
