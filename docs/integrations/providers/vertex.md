---
title: Vertex AI (Google Cloud)
description: Configure Contenox to use Gemini on Vertex AI — billed through your GCP project — and renew credentials when they expire.
---

# Vertex AI

The `vertex-google` backend runs **Gemini** on your own GCP project. Use it when you want Google-managed inference billed against your GCP account, regional control, or models that aren't on AI Studio.

> [!NOTE]
> For **Claude** or **Llama**, use a direct provider instead — [Anthropic](/docs/integrations/providers/anthropic/) or [AWS Bedrock](/docs/integrations/providers/bedrock/). Contenox does not support the Vertex Anthropic/Meta/Mistral partner backends.

## Prerequisites

1. A GCP project with billing enabled.
2. The Vertex AI API enabled on that project:

   ```bash
   gcloud services enable aiplatform.googleapis.com --project YOUR_PROJECT_ID
   ```

3. A region — e.g. `us-central1`, `europe-west4`. Each model is only available in some regions; check the [Vertex AI model availability matrix](https://cloud.google.com/vertex-ai/generative-ai/docs/learn/locations).

The backend URL always follows this shape:

```
https://{REGION}-aiplatform.googleapis.com/v1/projects/{PROJECT_ID}/locations/{REGION}
```

## Auth method 1 — Service account JSON (recommended for servers)

Create a service account, grant it the `roles/aiplatform.user` role, download a JSON key, and load it through an env var.

### Create the service account

```bash
# 1. Create the account
gcloud iam service-accounts create vertex-runner \
  --description="Service account for Contenox Vertex AI" \
  --display-name="Vertex Runner" \
  --project=YOUR_PROJECT_ID

# 2. Grant it the Vertex AI User role (required — without this every call 403s)
gcloud projects add-iam-policy-binding YOUR_PROJECT_ID \
  --member="serviceAccount:vertex-runner@YOUR_PROJECT_ID.iam.gserviceaccount.com" \
  --role="roles/aiplatform.user"

# 3. Generate and download the JSON key
gcloud iam service-accounts keys create service-account.json \
  --iam-account=vertex-runner@YOUR_PROJECT_ID.iam.gserviceaccount.com
```

> [!IMPORTANT]
> Step 2 is easy to skip. Creating the account and a key succeeds without it, but the service account has **no permissions** until the role is bound — inference then fails with `403 PERMISSION_DENIED`. If you already created the account, just run step 2 against the existing `vertex-runner@...` member.

Prefer the console? [GCP Console → IAM & Admin → Service Accounts](https://console.cloud.google.com/iam-admin/serviceaccounts) → **Create Service Account** → assign **Vertex AI User** → open the account → **Keys** tab → **Add Key → Create new key → JSON**. The file downloads once and Google keeps no backup — store it safely and never commit it.

### Wire it into Contenox

```bash
export VERTEX_SA_JSON=$(cat /path/to/service-account.json)

contenox backend add vertex --type vertex-google \
  --url "https://us-central1-aiplatform.googleapis.com/v1/projects/$GOOGLE_CLOUD_PROJECT/locations/us-central1" \
  --api-key-env VERTEX_SA_JSON

contenox config set default-model gemini-2.5-flash
contenox config set default-provider vertex-google
```

Contenox reads the JSON from the named env var at request time, so the key never lands in the config file on disk.

> [!NOTE]
> Use a concrete model id like `gemini-2.5-flash`, not an AI-Studio-style alias such as `gemini-flash-latest` — those `-latest` aliases exist on the Gemini API / AI Studio catalog but not on Vertex, and setting one as `default-model` here will fail. Run `contenox model list` after adding the backend to see what's actually available in your project/region.

## Auth method 2 — Application Default Credentials (CLI / dev only)

ADC reuses your `gcloud` login. It's the fastest way to try Vertex but expires when the refresh token is revoked (see [renewal](#renewing-credentials) below).

```bash
gcloud config set project YOUR_PROJECT_ID
gcloud services enable aiplatform.googleapis.com
gcloud auth application-default login
gcloud auth application-default set-quota-project YOUR_PROJECT_ID

contenox backend add vertex --type vertex-google \
  --url "https://us-central1-aiplatform.googleapis.com/v1/projects/YOUR_PROJECT_ID/locations/us-central1"

contenox config set default-model gemini-2.5-flash
contenox config set default-provider vertex-google
```

Omit `--api-key-env` and Contenox falls back to ADC.

> [!IMPORTANT]
> `set-quota-project` is required. Without it, every Vertex AI call returns `403 SERVICE_DISABLED` — even if you already ran `gcloud config set project`.

## Renewing credentials

Vertex tokens are short-lived (~1 hour). Contenox refreshes them automatically on every request, so day-to-day you never deal with the access token itself — what you renew is the **credential** the token is minted from.

### Symptom: `vertex AI token refresh: oauth2: "invalid_grant"`

This means ADC tried to mint a fresh access token from its refresh token and Google rejected the refresh token. Causes, most common first:

1. You haven't run `gcloud auth application-default login` in a long time and the refresh token aged out (Google rotates them).
2. You changed your Google account password.
3. The account's session was revoked (admin action, security policy, or you ran `gcloud auth application-default revoke`).
4. The ADC file at `~/.config/gcloud/application_default_credentials.json` was deleted or replaced by a different account.

Fix:

```bash
gcloud auth application-default login
gcloud auth application-default set-quota-project YOUR_PROJECT_ID
```

No `contenox` restart needed — the next request picks up the new credentials. If you removed the ADC file by hand, also re-run `set-quota-project`.

### Symptom: `vertex AI service account token: ...`

The service account JSON in `VERTEX_SA_JSON` is invalid, the key was disabled in GCP, or the service account was deleted. Generate a new key:

```bash
gcloud iam service-accounts keys create new-key.json \
  --iam-account=vertex-runner@YOUR_PROJECT_ID.iam.gserviceaccount.com

export VERTEX_SA_JSON=$(cat new-key.json)
```

Then restart the shell / process that holds the env var so Contenox sees the new value.

### Rotating a service account key on a schedule

Service account keys don't expire on their own — rotate them yourself. Typical pattern:

```bash
# Create a new key
gcloud iam service-accounts keys create new-key.json \
  --iam-account=vertex-runner@YOUR_PROJECT_ID.iam.gserviceaccount.com

# Swap the env var (in your secret store / systemd unit / k8s secret)
export VERTEX_SA_JSON=$(cat new-key.json)

# Disable the old key once requests are flowing on the new one
gcloud iam service-accounts keys list \
  --iam-account=vertex-runner@YOUR_PROJECT_ID.iam.gserviceaccount.com

gcloud iam service-accounts keys delete OLD_KEY_ID \
  --iam-account=vertex-runner@YOUR_PROJECT_ID.iam.gserviceaccount.com
```

## Troubleshooting

| Error | Cause | Fix |
|-------|-------|-----|
| `oauth2: "invalid_grant"` | ADC refresh token revoked / aged out | `gcloud auth application-default login` |
| `403 SERVICE_DISABLED` | Quota project not set, or Vertex AI API not enabled | `gcloud services enable aiplatform.googleapis.com` and `gcloud auth application-default set-quota-project ...` |
| `403 PERMISSION_DENIED` on a service account | Missing `roles/aiplatform.user` | Grant the role on the project |
| `404` on the model | Model not available in your region | Pick a region from the [model availability matrix](https://cloud.google.com/vertex-ai/generative-ai/docs/learn/locations) and recreate the backend with the right URL |
| `unreachable: vertex-google list models: ...` in `contenox model list` | Same as `invalid_grant` above — the catalog fetch refreshes tokens too | Renew per the section above; the backend stays registered |

## See also

- [Google Gemini (AI Studio)](/docs/integrations/providers/gemini/) — simpler API-key flow if you don't need GCP
- [Configuration reference](/docs/reference/config/)
- [CLI reference: `backend add`](/docs/reference/contenox-cli/)
