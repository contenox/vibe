# Chains

You do not write one to get one. An agent is a Markdown file with a YAML
frontmatter header in `.contenox/agents/`, and contenox generates the chain
behind it — [declaring agents](https://contenox.com/docs/guide/agents/) is the
front door.

This page is about the generated artifact: what it is for, and when you take
the pen yourself.

## Why Chains?

A naked agent loop is useful, but it is not enough when AI can touch real tools.

A Chain answers the questions a serious team has to ask before letting a model
act:

- What is the task?
- Which model or provider may be used?
- Which tools may the model call?
- Which commands or API operations are allowed?
- What must stop for human approval?
- What state, trace, and evidence does the run leave behind?
- Can the workflow be reviewed, committed, diffed, and run again?

In Contenox, a Chain is not a prompt pipeline. It is the reviewed execution
contract around an agent loop. The loop inside that contract is mapped, task by
task, in [The agentic loop](https://contenox.com/docs/guide/chains/agentic-loop/).

---

## What You Read, and Sometimes Write

A Chain is a single versioned file where every decision is a visible JSON key.
Prompts, provider routing, tool scope, command policy, retry policy, token
limits, loop budgets, and branches sit in one artifact — which is what makes it
the thing you read to see what an agent may do.

You take the pen when you need a guarantee rather than an instruction: a branch,
a different model per step, a recovery path, a declared point where a human
stands. A declaration says none of those.

```json
{
  "id": "review",
  "token_limit": 65536,
  "tasks": [
    {
      "id": "review",
      "handler": "chat_completion",
      "system_instruction": "You are a code reviewer. Analyze the diff, run tests if tools are available, then give a concise review.",
      "execute_config": {
        "model": "{{var:model}}",
        "provider": "{{var:provider}}",
        "tools": ["local_shell", "local_fs"],
        "tools_policies": {
          "local_shell": {
            "_allowed_commands": "go,make,npm,cargo,grep,cat",
            "_denied_commands": "sudo,su,dd,mkfs,fdisk,parted,shred"
          },
          "local_fs": {
            "_allowed_dir": ".",
            "_max_read_bytes": "262144"
          }
        },
        "retry_policy": {
          "max_attempts": 4,
          "initial_backoff": "1s",
          "max_backoff": "30s",
          "jitter": 0.25,
          "rate_limit_min_wait": "10s"
        }
      },
      "transition": {
        "branches": [
          {
            "operator": "edge_traversed_at_least",
            "edge": "review->run_tools",
            "when": "6",
            "goto": "end"
          },
          { "operator": "equals", "when": "tool_call", "goto": "run_tools" },
          { "operator": "default", "goto": "end" }
        ]
      }
    },
    {
      "id": "run_tools",
      "handler": "execute_tool_calls",
      "input_var": "review",
      "execute_config": {
        "tools": ["local_shell", "local_fs"]
      },
      "transition": {
        "branches": [
          { "operator": "default", "goto": "review" }
        ]
      }
    }
  ]
}
```

HITL is not a hidden toggle. Gated tool calls route through policy files such as
`hitl-policy-default.json`, `hitl-policy-strict.json`, and editor-specific ACP
policies. The Chain defines what the workflow can ask for; the active policy
decides what must pause for approval before execution.

Declare an agent:
**[contenox.com/docs/guide/agents](https://contenox.com/docs/guide/agents/)**.
Write a chain by hand:
**[contenox.com/docs/guide/first-chain](https://contenox.com/docs/guide/chains/writing-a-chain/)**.
