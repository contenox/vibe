#!/bin/bash

#
# This script monitors the real-time download progress of specified models
# from the contenox/vibe API and displays a progress bar.
#
# Usage: ./wait-for-models.sh "embed_model:tag" "task_model:tag" "chat_model:tag"
#

# --- Configuration ---
API_BASE_URL="http://localhost:8081"
PROGRESS_URL="${API_BASE_URL}/queue/inProgress"
TIMEOUT_SECONDS=600

# --- Cleanup Function ---
# This function is called when the script is interrupted (e.g., with Ctrl+C).
cleanup() {
  echo -e "\n\n🧹 Script interrupted. Exiting."
  # The parent script's exit will terminate the child curl process.
  exit 1
}

# Trap signals to call the cleanup function
trap cleanup INT TERM

# --- Helper Functions ---
# Function to print error messages and exit
error_exit() {
  echo ""
  echo "❌ Error: $1"
  echo ""
  exit 1
}

# --- Script Logic ---

# Check if model names are provided as arguments
if [ "$#" -ne 3 ]; then
  error_exit "Three model names required. Usage: $0 \"embed_model:tag\" \"task_model:tag\" \"chat_model:tag\""
fi

EMBED_MODEL="$1"
TASK_MODEL="$2"
CHAT_MODEL="$3"
REQUIRED_MODELS=("$EMBED_MODEL" "$TASK_MODEL" "$CHAT_MODEL")

# --- Pre-flight Checks ---
echo "🔎 Performing pre-flight checks..."

# 1. Check if the runtime API is available
if ! curl -s -f "${API_BASE_URL}/health" > /dev/null; then
  error_exit "Runtime API at ${API_BASE_URL} is not available. Please ensure services are running."
fi

# 2. Check if the 'local-ollama' backend has been created
response=$(curl -s -w "\n%{http_code}" "${API_BASE_URL}/backends")
http_code=$(echo "$response" | tail -n1)
body=$(echo "$response" | sed '$d')
if [ "$http_code" -ne 200 ]; then
    error_exit "Failed to get backends. API returned status ${http_code}."
fi
BACKEND_ID=$(echo "$body" | jq -r '.[] | select(.name=="local-ollama") | .id')
if [ -z "$BACKEND_ID" ]; then
  error_exit "Backend 'local-ollama' not found. Please run the bootstrap script or Step 2 from the README."
fi
echo "  ✅ Backend 'local-ollama' found (ID: $BACKEND_ID)."

# 3. Check if the backend is associated with the required groups
response=$(curl -s -w "\n%{http_code}" "${API_BASE_URL}/backend-affinity/internal_tasks_group/backends")
http_code=$(echo "$response" | tail -n1)
body=$(echo "$response" | sed '$d')
if [ "$http_code" -ne 200 ]; then error_exit "Failed to check task group associations. API returned ${http_code}."; fi
TASK_group_CHECK=$(echo "$body" | jq -r --arg BID "$BACKEND_ID" '(. // []) | .[] | select(.id==$BID) | .id // empty')

response=$(curl -s -w "\n%{http_code}" "${API_BASE_URL}/backend-affinity/internal_embed_group/backends")
http_code=$(echo "$response" | tail -n1)
body=$(echo "$response" | sed '$d')
if [ "$http_code" -ne 200 ]; then error_exit "Failed to check embed group associations. API returned ${http_code}."; fi
EMBED_group_CHECK=$(echo "$body" | jq -r --arg BID "$BACKEND_ID" '(. // []) | .[] | select(.id==$BID) | .id // empty')

response=$(curl -s -w "\n%{http_code}" "${API_BASE_URL}/backend-affinity/internal_chat_group/backends")
http_code=$(echo "$response" | tail -n1)
body=$(echo "$response" | sed '$d')
if [ "$http_code" -ne 200 ]; then error_exit "Failed to check chat group associations. API returned ${http_code}."; fi
CHAT_group_CHECK=$(echo "$body" | jq -r --arg BID "$BACKEND_ID" '(. // []) | .[] | select(.id==$BID) | .id // empty')

if [ -z "$TASK_group_CHECK" ] || [ -z "$EMBED_group_CHECK" ] || [ -z "$CHAT_group_CHECK" ]; then
  error_exit "Backend is not assigned to 'internal_tasks_group', 'internal_embed_group', and/or 'internal_chat_group'. Please run Step 3 from the README."
fi
echo "  ✅ Backend is correctly assigned to groups."

# 4. Check the runtime state of the backend for connection errors
response=$(curl -s -w "\n%{http_code}" "${API_BASE_URL}/state")
http_code=$(echo "$response" | tail -n1)
body=$(echo "$response" | sed '$d')
if [ "$http_code" -ne 200 ]; then
    error_exit "Failed to get runtime state. API returned status ${http_code}."
fi
BACKEND_STATE_ERROR=$(echo "$body" | jq -r --arg BID "$BACKEND_ID" '.[] | select(.id==$BID) | .error')
if [ "$BACKEND_STATE_ERROR" != "null" ] && [ -n "$BACKEND_STATE_ERROR" ]; then
    error_exit "The 'local-ollama' backend is reporting a runtime error: \"${BACKEND_STATE_ERROR}\""
fi
echo "  ✅ Backend runtime state is healthy."
echo ""

# --- Check for Existing Models ---
echo "🔎 Checking if models are already available..."
PULLED_MODELS=$(echo "$body" | jq -r --arg BID "$BACKEND_ID" '.[] | select(.id==$BID) | .pulledModels[].model' 2>/dev/null || echo "")

# Debug output
echo "  💡 Found pulled models:"
if [ -z "$PULLED_MODELS" ]; then
  echo "    (none)"
else
  echo "$PULLED_MODELS" | sed 's/^/    /'
fi

all_exist=true
for model in "${REQUIRED_MODELS[@]}"; do
  echo "  🔍 Checking for '$model'..."
  if ! printf "%s" "$PULLED_MODELS" | grep -q -x -F "$model"; then
    echo "    ❌ NOT FOUND"
    all_exist=false
    break
  else
    echo "    ✅ FOUND"
  fi
done

if [ "$all_exist" = true ]; then
  echo "✅ All required models are already available. Nothing to download."
  trap - INT TERM EXIT
  exit 0
fi
echo "  ⏳ Some models need to be downloaded. Starting progress monitor..."
echo ""

# --- Main Logic ---
echo "⏳ Waiting for the following models to download..."
declare -A models_done
for model in "${REQUIRED_MODELS[@]}"; do models_done["$model"]=0; done

check_all_done() {
  for model in "${REQUIRED_MODELS[@]}"; do if [[ ${models_done["$model"]} -eq 0 ]]; then return 1; fi; done
  return 0
}

while true; do
  if read -t 30 -r line; then
    if [[ $line == data:* ]]; then
      json_data=$(echo "$line" | sed 's/^data: //')
      model=$(echo "$json_data" | jq -r '.model')
      completed=$(echo "$json_data" | jq -r '.completed')
      total=$(echo "$json_data" | jq -r '.total')

      if [[ -v models_done["$model"] ]]; then
        if (( total > 0 )); then
          percent=$((completed * 100 / total))
          bar_width=40
          completed_width=$((percent * bar_width / 100))
          bar=$(printf "%-${bar_width}s" "$(printf '#%.0s' $(seq 1 $completed_width))")
          printf "  [%s] %3d%% - %s\r" "$bar" "$percent" "$model"

          if (( percent >= 100 && models_done["$model"] == 0 )); then
            models_done["$model"]=1
            printf "  [$(printf '#%.0s' $(seq 1 $bar_width))] 100%% - %s (Done)\n" "$model"
          fi
        fi

        if check_all_done; then
          echo -e "\n✅ All models are ready!"
          trap - INT TERM EXIT
          exit 0
        fi
      fi
    fi
  else
    echo -e "\n\n⏳ No download progress received for 30 seconds. Re-checking backend state..."
    response=$(curl -s -w "\n%{http_code}" "${API_BASE_URL}/state")
    http_code=$(echo "$response" | tail -n1)
    body=$(echo "$response" | sed '$d')
    if [ "$http_code" -ne 200 ]; then error_exit "Failed to get runtime state. API returned ${http_code}."; fi
    BACKEND_STATE_ERROR=$(echo "$body" | jq -r --arg BID "$BACKEND_ID" '.[] | select(.id==$BID) | .error')
    if [ "$BACKEND_STATE_ERROR" != "null" ] && [ -n "$BACKEND_STATE_ERROR" ]; then
        error_exit "The 'local-ollama' backend is now reporting a runtime error: \"${BACKEND_STATE_ERROR}\""
    else
        echo "  ✅ Backend is still healthy. Continuing to wait..."
    fi
  fi
done < <(timeout ${TIMEOUT_SECONDS}s curl -s -f -N "$PROGRESS_URL")

if check_all_done; then
  echo -e "\n✅ Models were successfully downloaded."
else
  echo -e "\n\n⚠️  Warning: Script finished before all models were confirmed downloaded."
  echo "Please check the backend status manually: curl -s http://localhost:8081/state | jq"
fi
