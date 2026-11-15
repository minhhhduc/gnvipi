export type Protocol = "chat" | "responses" | "messages";

export const PROTOCOLS: [Protocol, string, string][] = [
  ["chat", "OpenAI Chat Completion", "/v1/chat/completions"],
  ["responses", "OpenAI Responses", "/v1/responses"],
  ["messages", "Anthropic Messages", "/v1/messages"],
];

export const guideOrigin = typeof window === "undefined" ? "http://localhost:8080" : window.location.origin;

/**
 * Claude Code setup as a gateway-wide block for ~/.claude/settings.json.
 * automode plugs into auto-mode classification; the main model is untouched.
 */
export function claudeCodeSettings(origin: string): string {
  return `{
  "env": {
    "ANTHROPIC_BASE_URL": "${origin}",
    "ANTHROPIC_AUTH_TOKEN": "$GVIPI_GATEWAY_TOKEN",
    "ANTHROPIC_API_KEY": "",
    "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY": "1",
    "CLAUDE_CODE_AUTO_MODE_MODEL": "automode"
  }
}`;
}

export function curlSample(protocol: Protocol, model: string, origin = guideOrigin): string {
  if (protocol === "messages") {
    return `curl ${origin}/v1/messages \\
  -H "x-api-key: $GNVIPI_API_KEY" \\
  -H "anthropic-version: 2023-06-01" \\
  -H "content-type: application/json" \\
  -d '{
    "model": "${model}",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "Hello"}]
  }'`;
  }
  const body =
    protocol === "responses"
      ? `{
    "model": "${model}",
    "input": "Hello"
  }`
      : `{
    "model": "${model}",
    "messages": [{"role": "user", "content": "Hello"}]
  }`;
  const path = protocol === "responses" ? "/v1/responses" : "/v1/chat/completions";
  return `curl ${origin}${path} \\
  -H "Authorization: Bearer $GNVIPI_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '${body}'`;
}

export function codexConfig(model: string, origin = guideOrigin): string {
  return `model = "${model}"
model_provider = "gnvipi"

[model_providers.gnvipi]
name = "GNVipi gateway"
base_url = "${origin}/v1"
env_key = "GNVIPI_API_KEY"
wire_api = "responses"`;
}

export function openCodeConfig(model: string, origin = guideOrigin): string {
  return `{
  "provider": {
    "gnvipi": {
      "npm": "@ai-sdk/openai-compatible",
      "options": { "baseURL": "${origin}/v1", "apiKey": "{env:GNVIPI_API_KEY}" },
      "models": { "${model}": {} }
    }
  }
}`;
}