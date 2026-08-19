---
title: Leads → HubSpot
description: One agent finds leads through Tavily's MCP server, another writes them into HubSpot through a hand-curated three-operation OpenAPI subset — two declarations, two contenox run invocations, no glue script.
---

# Leads → HubSpot

Two declared agents, fired one after the other. The first finds fresh leads on
the web through Tavily's MCP server and writes them to a file; the second reads
that file and creates the companies, contacts and associations in HubSpot
through a hand-curated slice of its REST API.

The recipe is also a tour of the two ways contenox reaches a third party:

- **Part 1** uses a hosted **MCP server** with RFC 7591 dynamic OAuth — you
  authorize once in a browser and there is no app to configure.
- **Part 2** uses a hand-written **OpenAPI subset** against HubSpot's REST API —
  a bearer token, three operations, and no MCP server involved.

## Prerequisites

- `contenox init` in the project, and a model with tool calling — see
  [Quickstart](/docs/guide/quickstart/). A fast model earns its keep here: the
  work is batch tool calls, not reasoning, and a thinking-class model tends to
  second-guess search results instead of committing to them.
- An **envelope**. [`contenox run`](/docs/reference/contenox-cli/#contenox-run)
  fires a [mission](/docs/guide/missions/), and a mission that names no envelope
  is refused:

  ```bash
  contenox config set default-mission-policy hitl-policy-default.json
  ```

- A free [Tavily](https://www.tavily.com/) account, for Part 1.
- A HubSpot CRM portal you have admin access to, for Part 2.

---

# Part 1 — Find leads with Tavily

## 1.1 Register Tavily's MCP server

```bash
contenox mcp add tavily https://mcp.tavily.com/mcp/ --auth-type oauth
contenox mcp auth tavily
```

`mcp auth` is a separate, required step: `add` registers the server, it does not
authenticate it. The command opens your browser, and the local CLI catches the
redirect and persists the tokens. There is no `client_id` or `client_secret` to
manage — Tavily's MCP supports dynamic client registration, so contenox
negotiates everything at runtime.

```text
MCP server "tavily" added successfully.
Opening browser for contenox authorization...
tavily: authenticated successfully.
```

## 1.2 Declare the agent

`.contenox/agents/lead-finder.md`:

```markdown
---
name: lead-finder
description: Finds recent funding announcements through web search and writes them to a leads file
tools: Write, tavily.tavily_search
mcpServers: [tavily]
---

You research companies and write down what you found. You do not embellish it.

Search with tavily_search. Read the results before extracting anything from
them, and take the company name, the founder or CEO, and one sentence on what
the company does — all three from the article, never from what you already
believe about the company.

Write the result to the path you were given, as blank-line-separated blocks in
exactly this shape and no other:

1. Company Name: <name>
   Founder/CEO: <name>[ and <name>]
   Summary: <one sentence>

A lead you could not verify from a result you actually read is left out, and
the count you report says how many you left out and why. A plausible company
that does not exist is worse than four leads instead of five.
```

Two tools, both named. `mcpServers: [tavily]` is the grant — this agent may
reach that server and nothing else new — and `tavily.tavily_search` names the
one operation it uses from it. `Write` is `local_fs.write_file`; there is no
`Read` and no `Bash` on this agent, because finding leads needs neither.

## 1.3 Run it

```bash
contenox run lead-finder \
  "Find 5 recent announcements of B2B SaaS startups in London that raised Seed funding. Write them to leads.txt." \
  --model gemini-flash-latest --provider gemini --timeout 5m
```

`leads.txt` comes out looking like this:

```text
1. Company Name: Acme Robotics
   Founder/CEO: Jordan Lee
   Summary: A cybersecurity SaaS platform that helps organizations continuously detect, assess, and manage third-party and supply-chain cyber risks in real time.

2. Company Name: Northwind Traders
   Founder/CEO: Morgan Reyes and Alex Kim
   Summary: Agent-first billing and payments infrastructure for AI-native businesses that monetize usage and outcomes.

3. Company Name: Globex Treasury
   Founder/CEO: Taylor Brooks and Sam Patel
   Summary: An AI-powered finance automation platform covering treasury, accounts payable, payroll and FX for B2B finance teams.
```

> **If the run waits instead of finishing:** `contenox run` has no terminal in
> front of it, so a gated call becomes a
> [durable ask](/docs/guide/hitl/#the-life-of-an-ask) rather than
> a prompt, and the run blocks on that row. `contenox approvals list` shows it;
> `contenox approvals respond <ask-id> --approve` releases it — the waiting call
> if the run is still up, its checkpoint if `--timeout` already ended it. If you would rather
> it never stopped, that belongs in the envelope — see
> [Unattended writes](#unattended-writes), below.

---

# Part 2 — Push leads into HubSpot

## 2.1 Get a HubSpot credential

You need a bearer token with these CRM scopes:

- `crm.objects.companies.read`
- `crm.objects.companies.write`
- `crm.objects.contacts.read`
- `crm.objects.contacts.write`

Two routes produce a `pat-*` token that authenticates as
`Authorization: Bearer <token>`:

- **Private App** — in your CRM portal: gear icon → Integrations → Private Apps →
  "Create private app". GA, and the one to use.
- **Service Key** — `developer.hubspot.com` → Service Keys (public beta). Same
  auth shape, same scopes, narrower scope picker.

```bash
export HUBSPOT_TOKEN=pat-na1-...
```

> HubSpot also ships an OAuth MCP server with its full curated CRM tool set —
> see [HubSpot via MCP](/docs/use-cases/hubspot-mcp/). The OpenAPI route below is
> the right pick when you want a narrow, hand-curated surface (three operations
> against HubSpot's dozen) and finer control over what can be written. It is also
> the pattern for any vendor that ships a spec and no MCP server.

## 2.2 Drop the OpenAPI subset

Save to `~/.contenox/hubspot-revops.yaml`. Three operations out of HubSpot's CRM
v3 API — small enough to keep the model on the rails, and everything this recipe
needs.

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

## 2.3 Register it as a tool

```bash
contenox tools add hubspot \
    --url https://api.hubapi.com \
    --spec ~/.contenox/hubspot-revops.yaml \
    --header "Authorization: Bearer $HUBSPOT_TOKEN"

contenox tools show hubspot
```

`tools show` lists `searchCompany`, `createCompany` and `createContact`. The
credential rides in a header contenox injects on every call — the model never
sees it and cannot put it anywhere.

## 2.4 Declare the agent

`.contenox/agents/revops.md`:

```markdown
---
name: revops
description: Creates HubSpot companies and contacts from a structured leads file, skipping duplicates
tools: Read, hubspot.searchCompany, hubspot.createCompany, hubspot.createContact
---

You load leads into HubSpot. Every write is irreversible from here, so read
before you write and never write twice.

Read the leads file you were given. It holds blank-line-separated blocks, each
with a Company Name, one or more Founder/CEO names, and a Summary.

For each block, in order:

1. searchCompany with
   filterGroups=[{filters:[{propertyName:"name", operator:"EQ", value:<company name>}]}].
   If results is non-empty, record "SKIP <company name>: already in HubSpot" and
   move to the next block. Do not search twice for the same name.
2. createCompany with properties.name=<company name> and
   properties.description=<summary>. Keep the returned id.
3. For each Founder/CEO (split the field on " and "), createContact with
   properties.firstname, properties.lastname, properties.jobtitle="Founder/CEO",
   and associations=[{"to":{"id":<company id>},
   "types":[{"associationCategory":"HUBSPOT_DEFINED","associationTypeId":279}]}].

Never invent a property the block did not give you. If a call fails, stop at
that block, report which one and what the error said, and do not continue past
it — a half-loaded CRM you can see the boundary of is recoverable; one you
cannot is not.

REPORT one line per block: "OK <company name> id=<id> contacts=<ids>" or
"SKIP <company name>: <reason>" or "FAIL <company name>: <error>", then a count
of each.
```

`Read` is the only local tool it has. It cannot write to disk, it cannot reach
the shell, and the three dotted names are the entire slice of HubSpot it can
touch — the spec you wrote in 2.2 is the outer bound, and the declaration
narrows it further.

## 2.5 Run it

```bash
contenox run revops "load every lead in leads.txt into HubSpot" \
  --model gemini-flash-latest --provider gemini --timeout 10m
```

```text
OK Acme Robotics id=100000000001 contacts=200000000001
OK Northwind Traders id=100000000002 contacts=200000000002,200000000003
OK Globex Treasury id=100000000003 contacts=200000000004,200000000005
SKIP Initech: already in HubSpot
3 created, 1 skipped, 0 failed
```

One mission for the whole file. The bounds it works inside — the tool-call
ceiling, the loop rounds — are in
[`agents.toml`](/docs/reference/agents-config/) (`[policy.compute]
max_tool_calls`, `[chain] main_rounds`), and a leads file large enough to
outgrow them is split by the caller, one run per chunk:

```bash
split -l 40 leads.txt chunk-
for f in chunk-*; do contenox run revops "load every lead in $f into HubSpot"; done
```

## 2.6 Verify what landed

```bash
curl -sS -H "Authorization: Bearer $HUBSPOT_TOKEN" \
  "https://api.hubapi.com/crm/v3/objects/companies?limit=20&properties=name,description&sort=-createdate" \
  | python3 -m json.tool
```

The new companies come back at the top with descriptions matching the `Summary:`
lines. Same at `/crm/v3/objects/contacts` for the founders — `firstname`,
`lastname`, `jobtitle="Founder/CEO"`, each associated to its company.

The run itself is on the record too:

```bash
contenox mission list
contenox mission reports <mission-id>
```

---

## Unattended writes

Naming a tool in a declaration makes it reachable, not permitted. `hubspot`
matches no rule in the shipped policy, so it falls through to `default_action` —
`approve` — and asks a human on every call. At a keyboard that is an approval
card. Under `contenox run` there is nobody to ask, so it is a durable ask and the
run waits on it.

If these writes should run unattended, say so once in `agents.toml`:

```toml
[[policy.always_allow]]
tools = "hubspot"
tool = "createCompany"

[[policy.always_allow]]
tools = "hubspot"
tool = "createContact"
```

Grants are emitted after the shipped denies, so a rule here can never reach a
credential path no matter how broadly it is written.

Whether to grant this is a real decision, not a formality: `createCompany` writes
to your CRM, and the agent deciding to call it is reading text a stranger
published on the web. Granting `searchCompany` and leaving the two creates gated
is a defensible middle — the run waits on each write, and
`contenox approvals list` is your queue.

---

## Customize

- **A different lead source.** Swap Tavily for any other search MCP (Perplexity
  Sonar, Exa, You.com): register it, change the `mcpServers` grant and the tool
  name in `lead-finder.md`, leave the prompt alone. Or skip Part 1 and bring a
  `leads.txt` from anywhere.
- **More fields.** Map extra properties — `domain`, `industry`, `city` — in the
  `revops` prompt. No spec change: `properties` is an open map.
- **A notes step.** Add a `createNote` operation to the spec (path
  `/crm/v3/objects/notes`, same `CreateInput` shape; `associationTypeId` 190 for
  Note→Company, 202 for Note→Contact), then add its dotted name to the
  declaration's `tools`. Needs the `crm.objects.notes.write` scope, which Private
  Apps have and Service Keys currently do not.
- **A different model.** `--model` and `--provider` per invocation; any
  tool-calling model works.

## Caveats

- **Tavily's free tier** is about $5 in monthly credits. Fine for occasional lead
  discovery; watch it if you schedule the run.
- **HubSpot Service Keys are public beta** and their scope picker does not expose
  `crm.objects.notes.*`. Use a Private App if you want the notes step.
- **The token lives in your database.** `HUBSPOT_TOKEN` is stored in the
  `remote_tools` row's headers in `~/.contenox/local.db`, on this machine —
  rotate it the way you rotate any other stored credential. Tavily's OAuth tokens
  sit under the MCP server's row.
- **Duplicate detection is `EQ` on the name.** "Acme Robotics" and "Acme
  Robotics Ltd" are two companies as far as this recipe is concerned. Tighten the
  step-1 filter if your portal cares.
