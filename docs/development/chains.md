# Chains

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
task, in [The agentic loop](https://contenox.com/docs/guide/agentic-loop/).

![A sudo command is refused because the chain's command policy denies it; the policy is plain JSON](/chain-blocked.gif)

---

## What You Author

The unit of work is a Chain: a single versioned file where every decision is a
visible JSON key. Prompts, provider routing, tool scope, command policy, retry
policy, token limits, loop budgets, and branches are part of the artifact you
review.

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

Save it, then pipe your work into it. It speaks Unix:

```bash
git diff | contenox run --chain ./review.json
```

HITL is not a hidden toggle. Gated tool calls route through policy files such as
`hitl-policy-default.json`, `hitl-policy-strict.json`, and editor-specific ACP
policies. The Chain defines what the workflow can ask for; the active policy
decides what must pause for approval before execution.

Walk through your first chain:
**[contenox.com/docs/guide/first-chain](https://contenox.com/docs/guide/first-chain/)**.
