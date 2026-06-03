#!/bin/sh
# Render the runtime config from the template, substituting only the secret/
# env-provided values, then exec the reconciler with whatever args the compose
# command supplies (e.g. -interval 4h -jitter 10m).
#
# Secrets (Anthropic + arr API keys) live in the compose .env and arrive as
# environment variables; they are written to /etc/arr-reconciler/config.json at
# container start and never committed to the repo or baked into the image.
set -eu

TEMPLATE=/etc/arr-reconciler/config.template.json
RENDERED=/etc/arr-reconciler/config.json

# Fail fast with a clear message if a required secret is missing, rather than
# rendering an empty api_key that only fails later at validation.
: "${CLAUDE_API_KEY:?CLAUDE_API_KEY (PLEX_LLM_API_KEY in .env) is required}"
: "${SONARR_API_KEY:?SONARR_API_KEY is required}"
: "${RADARR_API_KEY:?RADARR_API_KEY is required}"

# Optional Cloudflare AI Gateway routing. Default the base URL to the public
# Anthropic API and the gateway token to empty, so the gateway is opt-in: if
# CLAUDE_GATEWAY_TOKEN is unset the client just talks to Anthropic directly.
: "${CLAUDE_BASE_URL:=https://api.anthropic.com}"
: "${CLAUDE_GATEWAY_TOKEN:=}"

# Optional local-model prefilter tier (Ollama). Empty disables it: every
# candidate then escalates straight to the frontier model.
: "${LOCAL_ENDPOINT:=}"

# Anthropic Message Batches API (50% cheaper, async). Default off (synchronous).
# Must render to a bare JSON boolean (true/false), so default it explicitly.
: "${CLAUDE_BATCH_MODE:=false}"

# Only substitute the variables we explicitly name, so a literal $ elsewhere in
# the template is left untouched.
envsubst '${CLAUDE_API_KEY} ${CLAUDE_BASE_URL} ${CLAUDE_GATEWAY_TOKEN} ${SONARR_API_KEY} ${RADARR_API_KEY} ${LLMOBS_ENDPOINT} ${ML_APP} ${LOCAL_ENDPOINT} ${CLAUDE_BATCH_MODE}' \
	< "$TEMPLATE" > "$RENDERED"

exec /usr/local/bin/arr-reconciler -config "$RENDERED" "$@"
