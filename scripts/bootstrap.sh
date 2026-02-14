#!/bin/bash

#
# This script automates the entire bootstrapping process for the contenox/vibe.
# It starts the necessary services, configures the backend and affinity groups, and
# ensures the required models are downloaded and ready for use.
#
# Usage:
#   ./scripts/bootstrap.sh [embed_model] [task_model] [chat_model]
#   (e.g., ./bootstrap.sh nomic-embed-text:latest phi3:3.8b phi3:3.8b)
#
# Three models are required: embedding model, task model, and chat model
#

# Exit immediately if a command exits with a non-zero status.
set -e

# --- Helper Functions ---
# Function to print messages
log() {
  echo "➡️  $1"
}

# Function to print success messages
success() {
  echo "✅ $1"
}

# Function to print error messages and exit
error_exit() {
  echo ""
  echo "❌ Error: $1"
  echo ""
  exit 1
}

# --- Configuration ---
API_BASE_URL="http://localhost:8081"

# --- Process Command-Line Arguments ---
# If arguments provided, use them as models; otherwise use defaults
if [ $# -eq 3 ]; then
  log "Using command-line specified models:"
  log "  Embedding: $1"
  log "  Task: $2"
  log "  Chat: $3"
  EMBED_MODEL="$1"
  TASK_MODEL="$2"
  CHAT_MODEL="$3"
else
  error_exit "Three models are required: embedding model, task model, and chat model"
fi

# Validate all models are non-empty
for model in "$EMBED_MODEL" "$TASK_MODEL" "$CHAT_MODEL"; do
  if [ -z "$model" ]; then
    error_exit "Empty model name detected. Please provide non-empty model names."
  fi
done

REQUIRED_MODELS=("$EMBED_MODEL" "$TASK_MODEL" "$CHAT_MODEL")

# --- Main Logic ---

# 1. Check for dependencies
log "Checking for required tools (docker, curl, jq)..."
for tool in docker curl jq; do
  if ! command -v $tool &> /dev/null; then
    error_exit "'$tool' is not installed. Please install it to continue."
  fi
done
success "All tools are available."

# 3. Wait for the runtime API to be healthy
log "Waiting for the runtime API to become healthy..."
ATTEMPTS=0
MAX_ATTEMPTS=60 # Wait for up to 60 seconds
while ! curl -s -f "${API_BASE_URL}/health" > /dev/null; do
  ATTEMPTS=$((ATTEMPTS + 1))
  if [ $ATTEMPTS -ge $MAX_ATTEMPTS ]; then
    error_exit "Runtime API did not become healthy after $MAX_ATTEMPTS seconds. Please check the container logs with 'docker logs contenox-vibe-kernel'."
  fi
  sleep 1
done
success "Runtime API is healthy and responding."

log "Checking runtime version..."
VERSION=$(curl -s -f "${API_BASE_URL}/version" | jq -r .version)
success "Runtime version is $VERSION."

# 4. Register the 'local-ollama' backend if it doesn't exist
log "Checking for 'local-ollama' backend..."
response=$(curl -s -w "\n%{http_code}" "${API_BASE_URL}/backends")
http_code=$(echo "$response" | tail -n1)
body=$(echo "$response" | sed '$d')

if [ "$http_code" -ne 200 ]; then
    error_exit "Failed to get backends. API returned status ${http_code}."
fi
BACKEND_ID=$(echo "$body" | jq -r '(. // []) | .[] | select(.name=="local-ollama") | .id')
# Add this after the environment variable section but before backend registration
OLLAMA_BACKEND_URL="${OLLAMA_BACKEND_URL:-http://ollama:11434}"

# Then modify the backend registration section:
if [ -z "$BACKEND_ID" ]; then
  log "Backend not found. Registering 'local-ollama'..."
  response=$(curl -s -w "\n%{http_code}" -X POST ${API_BASE_URL}/backends \
    -H "Content-Type: application/json" \
    -d '{
      "name": "local-ollama",
      "baseURL": "'"$OLLAMA_BACKEND_URL"'",
      "type": "ollama"
    }')
  http_code=$(echo "$response" | tail -n1)
  body=$(echo "$response" | sed '$d')

  if [ "$http_code" -ne 201 ]; then
    error_exit "Failed to register backend. API returned status ${http_code} with body: $body"
  fi
  BACKEND_ID=$(echo "$body" | jq -r '.id')

  if [ -z "$BACKEND_ID" ] || [ "$BACKEND_ID" == "null" ]; then
    error_exit "Failed to register backend (ID was null). Please check the runtime logs."
  fi
  success "Backend 'local-ollama' registered with ID: $BACKEND_ID"
else
  success "Backend 'local-ollama' already exists with ID: $BACKEND_ID"
fi

# 5. Assign backend to default affinity groups if not already assigned
log "Assigning backend to default affinity groups..."
# group 1: internal_tasks_group
response=$(curl -s -w "\n%{http_code}" "${API_BASE_URL}/backend-affinity/internal_tasks_group/backends")
http_code=$(echo "$response" | tail -n1)
body=$(echo "$response" | sed '$d')
if [ "$http_code" -ne 200 ]; then
    error_exit "Failed to check task affinity group associations. API returned status ${http_code}."
fi
TASK_group_CHECK=$(echo "$body" | jq -r --arg BID "$BACKEND_ID" '(. // []) | .[] | select(.id==$BID) | .id')

if [ -z "$TASK_group_CHECK" ]; then
  http_code=$(curl -s -o /dev/null -w "%{http_code}" -X POST "${API_BASE_URL}/backend-affinity/internal_tasks_group/backends/$BACKEND_ID")
  if [ "$http_code" -ne 201 ] && [ "$http_code" -ne 200 ]; then
      error_exit "Failed to assign backend to task affinity group. API returned status ${http_code}."
  fi
  success "Assigned backend to 'internal_tasks_group'."
else
  success "Backend already assigned to 'internal_tasks_group'."
fi

# group 2: internal_embed_group
response=$(curl -s -w "\n%{http_code}" "${API_BASE_URL}/backend-affinity/internal_embed_group/backends")
http_code=$(echo "$response" | tail -n1)
body=$(echo "$response" | sed '$d')
if [ "$http_code" -ne 200 ]; then
    error_exit "Failed to check embed affinity group associations. API returned status ${http_code}."
fi
EMBED_group_CHECK=$(echo "$body" | jq -r --arg BID "$BACKEND_ID" '(. // []) | .[] | select(.id==$BID) | .id')

if [ -z "$EMBED_group_CHECK" ]; then
  http_code=$(curl -s -o /dev/null -w "%{http_code}" -X POST "${API_BASE_URL}/backend-affinity/internal_embed_group/backends/$BACKEND_ID")
  if [ "$http_code" -ne 201 ] && [ "$http_code" -ne 200 ]; then
      error_exit "Failed to assign backend to embed affinity group. API returned status ${http_code}."
  fi
  success "Assigned backend to 'internal_embed_group'."
else
  success "Backend already assigned to 'internal_embed_group'."
fi

# group 2: internal_chat_group
response=$(curl -s -w "\n%{http_code}" "${API_BASE_URL}/backend-affinity/internal_chat_group/backends")
http_code=$(echo "$response" | tail -n1)
body=$(echo "$response" | sed '$d')
if [ "$http_code" -ne 200 ]; then
    error_exit "Failed to check chat affinity group associations. API returned status ${http_code}."
fi
CHAT_group_CHECK=$(echo "$body" | jq -r --arg BID "$BACKEND_ID" '(. // []) | .[] | select(.id==$BID) | .id')

if [ -z "$CHAT_group_CHECK" ]; then
  http_code=$(curl -s -o /dev/null -w "%{http_code}" -X POST "${API_BASE_URL}/backend-affinity/internal_chat_group/backends/$BACKEND_ID")
  # Treat 409 (Conflict) as success since it means the backend is already assigned
  if [ "$http_code" -ne 201 ] && [ "$http_code" -ne 200 ] && [ "$http_code" -ne 409 ]; then
      error_exit "Failed to assign backend to chat affinity group. API returned status ${http_code}."
  fi
  success "Assigned backend to 'internal_chat_group'."
else
  success "Backend already assigned to 'internal_chat_group'."
fi

# 6. Wait for models to be downloaded
log "Handing off to model download monitor..."
# Ensure the wait script is executable
log "Waiting for affinity group assignments to propagate..."
sleep 2
log "Handing off to model download monitor..."
./scripts/wait-for-models.sh "${REQUIRED_MODELS[@]}"

# Final success message
echo ""
echo "🎉 Bootstrap complete! Your contenox/vibe environment is ready to use."
echo "   Using models:"
echo "   - Embedding: ${EMBED_MODEL}"
echo "   - Task: ${TASK_MODEL}"
echo "   - Chat: ${CHAT_MODEL}"
