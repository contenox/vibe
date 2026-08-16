---
title: The nested permission bomb
description: Why inheriting human permissions is a privilege escalation vulnerability, and how to author actual AI boundaries.
---

# The nested permission bomb

Give a workflow its own scoped credential and an authored HITL policy, instead of letting it inherit the operator's full access.

Reusing a human's own credentials for an automated workflow is a common shortcut, but it turns a prompt injection into direct production access, makes your audit log attribute the workflow's actions to a person, and creates unbounded delegation with no permission boundary. Excessive agency and over-privileged automation are named risks in both the NIST AI Risk Management Framework and the ACSC secure-AI guidance. The fix is to author the workflow's identity and its boundary explicitly, rather than inherit them.

## Prerequisites

- `contenox` installed.
- An API, or a filesystem/shell workflow, you want an assistant to use with its own scoped access.

## Steps

1. Register the tool with a service-level credential — not your personal token — and inject a caller identity hidden from the model:

   ```bash
   contenox tools add warehouse-api \
     --url https://api.internal.com \
     --header "Authorization: Bearer $WORKFLOW_SCOPED_TOKEN" \
     --inject "caller_id=workflow-finance-01" \
     --inject "invoked_by=tom"
   ```

   `--inject` values are stripped from the tool schema before the model ever sees it; the engine merges them into every call at execution time. The model can't read, tamper with, or omit them, and your logs now attribute the call to the workflow identity, not to whoever happened to be logged in.

2. For a large API, register only a hand-curated subset rather than the whole surface — see [Authoring your tool inventory](/docs/use-cases/openapi-subset/) for the `--spec` flag in full. A workflow whose job is reading warehouse stock shouldn't also have a `delete_shipment` tool just because the vendor's OpenAPI spec includes it.

3. Author a HITL policy that denies credential/secret paths outright, requires approval for writes, and allows harmless reads without interruption:

   ```json
   {
     "default_action": "approve",
     "rules": [
       {"tools": "local_fs", "tool": "*", "action": "deny", "when": [{"key": "path", "op": "glob", "value": "**/{.ssh,.gnupg,.aws,.azure,.kube,.config/gcloud}/**"}]},
       {"tools": "local_fs", "tool": "*", "action": "deny", "when": [{"key": "path", "op": "glob", "value": "**/{.password-store,.local/share/keyrings,Library/Keychains,.config/1Password}/**"}]},
       {"tools": "local_fs", "tool": "*", "action": "deny", "when": [{"key": "path", "op": "glob", "value": "**/{.bash_history,.zsh_history,.netrc,.npmrc}"}]},

       {"tools": "local_shell", "tool": "local_shell", "action": "deny"},

       {"tools": "local_fs", "tool": "write_file", "action": "approve"},
       {"tools": "local_fs", "tool": "sed", "action": "approve"},

       {"tools": "local_fs", "tool": "read_file", "action": "allow"}
     ]
   }
   ```

   Denying `local_shell` outright also removes the only path to a registered API tool's network call if that tool were shell-driven — but tools registered via `contenox tools add` (step 1) call out directly, not through the shell, so they're unaffected by this rule and stay governed by their own entry in this policy.

4. Save it and activate it:

   ```bash
   contenox config set hitl-policy-name hitl-policy-<name>.json
   ```

## Expected outcome

- Logs show the workflow's own identity (`caller_id`, `invoked_by`) on every call, not a human's.
- Reads of `.ssh`, `.aws`, shell history, and similar credential paths are denied outright.
- `local_shell` is denied entirely under this policy; `write_file` and `sed` pause for approval; `read_file` passes silently.
- Revoking the workflow's access means rotating its own scoped token — no human's credentials are touched.

## Where to next

- [Any API, a tool you authored](/docs/use-cases/any-api-as-a-tool/) — the same `--header`/`--inject`/`--spec` mechanism, worked end to end.
- [Authoring your tool inventory](/docs/use-cases/openapi-subset/) — curating a narrow OpenAPI subset instead of registering a whole API.
- [The pause is yours to define](/docs/use-cases/authored-approval/) — writing and switching HITL policies.
- [HITL policies](/docs/guide/hitl/) — the full policy schema.
