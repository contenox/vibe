---
title: AWS Bedrock
description: Connect Contenox to Amazon Bedrock — Claude, Llama, Mistral, and Nova models on your AWS account.
---

# AWS Bedrock

Amazon Bedrock hosts Claude, Llama, Mistral, and Amazon Nova models on your AWS account. Contenox calls the Bedrock **Converse** API; credentials come from the standard AWS chain, and the region is carried in the backend `--url`.

## Prerequisites

1. An AWS account with Bedrock available in your region.
2. **Enable the models you want** in the Bedrock console → **Model access**. Until a model is enabled, every call returns `AccessDeniedException` — even though the model still appears in `contenox model list`.
3. Working AWS credentials (see auth below). Verify with:

   ```bash
   aws sts get-caller-identity
   ```

## Auth — the ambient AWS credential chain

Contenox uses the standard AWS SDK credential chain: environment variables, a shared profile (`~/.aws/credentials`), or an IAM role (EC2/ECS/EKS/IMDS). No `--api-key-env` is needed.

```bash
# Example: env-var credentials (or use a profile: export AWS_PROFILE=my-profile)
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...

contenox backend add bedrock --type bedrock \
  --url "https://bedrock-runtime.us-east-1.amazonaws.com"          # the region lives in the URL

contenox model list                                                 # live: Converse-capable models in your account/region
contenox config set default-model us.anthropic.claude-3-5-sonnet-20241022-v2:0   # example — use an enabled id
contenox config set default-provider bedrock
```

The `--url` carries the region (`bedrock-runtime.<region>.amazonaws.com`); a bare region like `us-east-1` also works. The IAM principal needs `bedrock:InvokeModel` (and `bedrock:InvokeModelWithResponseStream` for streaming).

### Static keys instead of the ambient chain

To pin specific credentials, store a JSON blob in an env var and pass `--api-key-env`:

```bash
export AWS_BEDROCK_CREDS='{"access_key_id":"AKIA...","secret_access_key":"...","session_token":""}'

contenox backend add bedrock --type bedrock \
  --url "https://bedrock-runtime.us-east-1.amazonaws.com" --api-key-env AWS_BEDROCK_CREDS
```

## Model ids and inference profiles

`contenox model list` queries Bedrock live (`ListFoundationModels`) and shows the Converse-capable model ids available in your account and region (e.g. `anthropic.claude-3-5-sonnet-20241022-v2:0`) — it is not a fixed curated list. **Any** Bedrock model id your account can invoke works as `default-model`, whether or not it appears in the listing.

Most current Claude/Llama models can't be invoked on-demand by their bare foundation id — they require a **regional inference profile**. If a call fails with `ValidationException: ... on-demand throughput isn't supported ... use an inference profile`, prefix the id with your region group — `us.`, `eu.`, or `apac.`:

```bash
contenox config set default-model us.anthropic.claude-3-5-sonnet-20241022-v2:0
```

## EU regions

Bedrock is available in EU regions including Frankfurt (`eu-central-1`), Ireland (`eu-west-1`), London (`eu-west-2`), Paris (`eu-west-3`), Zurich (`eu-central-2`), Stockholm (`eu-north-1`), Milan (`eu-south-1`), and Spain (`eu-south-2`). As always, the region lives in the backend `--url`:

```bash
contenox backend add bedrock-eu --type bedrock \
  --url "https://bedrock-runtime.eu-central-1.amazonaws.com"     # Frankfurt
```

For models that require an inference profile, use the `eu.` prefix — an EU (geography-tied) profile routes requests only among its EU destination regions, listed on the model's detail page in the AWS docs:

```bash
contenox config set default-model eu.anthropic.claude-3-5-sonnet-20240620-v1:0   # example — use an enabled id
```

Model access is granted **per region**: enable the models you want in the Bedrock console → **Model access** for that region, then run `contenox model list` to see what your account can invoke there. See [AI sovereignty & the EU AI Act](/docs/guide/sovereignty/) for how region pinning fits the larger deployment posture.

## Troubleshooting

| Error | Cause | Fix |
|-------|-------|-----|
| `AccessDeniedException` on a model | Model not enabled for your account | Enable it in Bedrock console → **Model access** |
| `ValidationException: on-demand throughput isn't supported` | Bare foundation id needs an inference profile | Prefix with `us.` / `eu.` / `apac.` (see above) |
| `could not load credentials` / `no EC2 IMDS role found` | AWS chain found no credentials | Set `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` or `AWS_PROFILE`; confirm with `aws sts get-caller-identity` |
| `UnrecognizedClientException` | Keys invalid, or wrong region for the model | Check the keys and that the `--url` region matches where the model is enabled |

## See also

- [Anthropic (direct API)](/docs/integrations/providers/anthropic/) — Claude without an AWS account
- [Configuration reference](/docs/reference/config/)
- [CLI reference: `backend add`](/docs/reference/contenox-cli/)
