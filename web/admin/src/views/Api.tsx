import { useState } from "preact/hooks";
import { Card, CardHead, PageHeader, useUi } from "../components/ui";
import { useGateway } from "../lib/context";

type Protocol = "chat" | "responses" | "messages";

const PROTOCOLS: [Protocol, string, string][] = [
  ["chat", "OpenAI Chat Completion", "/v1/chat/completions"],
  ["responses", "OpenAI Responses", "/v1/responses"],
  ["messages", "Anthropic Messages", "/v1/messages"],
];

const origin = typeof window === "undefined" ? "http://localhost:8080" : window.location.origin;

/** Claude Code env: automode plugs into auto-mode classification, not the main model. */
export function claudeCodeEnv(origin: string): string {
  return `export ANTHROPIC_BASE_URL="${origin}"
export ANTHROPIC_API_KEY="$GNVIPI_API_KEY"
export CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY="1"
export CLAUDE_CODE_AUTO_MODE_MODEL="automode"`;
}

export function curlSample(protocol: Protocol, model: string): string {
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

function Snippet({ label, code }: { label: string; code: string }) {
  const { toast } = useUi();
  const copy = () => {
    navigator.clipboard.writeText(code).then(
      () => toast("Copied"),
      () => toast("Copy failed", "bad"),
    );
  };
  return (
    <div class="mb-3">
      <div class="mb-1 flex items-center justify-between">
        <span class="text-[11px] font-semibold uppercase tracking-[0.05em] text-dim">{label}</span>
        <button type="button" class="btn btn-sm" onClick={copy}>Copy</button>
      </div>
      <pre class="mono overflow-x-auto rounded-lg border border-line bg-panel2 px-3 py-2.5 text-[11px] leading-5">{code}</pre>
    </div>
  );
}

export function ApiView() {
  const { navigate } = useGateway();
  const [protocol, setProtocol] = useState<Protocol>("chat");
  const [model, setModel] = useState("automode");
  const path = PROTOCOLS.find(([key]) => key === protocol)?.[2] ?? "";

  return (
    <>
      <PageHeader title="API" sub="Endpoints, sample code and agent setup" />

      <Card>
        <CardHead title="Base URL" sub="Every protocol below is served from this address." />
        <div class="px-[15px] py-3.5">
          <Snippet label="Base URL" code={origin} />
          <table class="table">
            <tbody>
              {PROTOCOLS.map(([key, label, endpoint]) => (
                <tr key={key}>
                  <td class="w-[220px] font-semibold">{label}</td>
                  <td class="mono text-xs">POST {endpoint}</td>
                </tr>
              ))}
            </tbody>
          </table>
          <p class="mt-2 text-[11px] text-dim">
            Authenticate with a gateway API key from the <a href="?page=tokens" onClick={(e) => { e.preventDefault(); navigate("tokens"); }}>API Keys</a> page:
            <span class="mono"> Authorization: Bearer $GNVIPI_API_KEY</span> (or <span class="mono">x-api-key</span> for Anthropic clients).
          </p>
        </div>
      </Card>

      <Card>
        <CardHead title="Sample code" sub="Pick a protocol; the model id is editable." />
        <div class="px-[15px] py-3.5">
          <div class="mb-3 grid grid-cols-3 gap-1 rounded-xl border border-line bg-panel2 p-1">
            {PROTOCOLS.map(([key, label]) => (
              <button
                key={key}
                type="button"
                class={`rounded-lg px-2 py-1.5 text-xs font-medium ${
                  protocol === key ? "bg-panel font-semibold text-fg shadow-sm" : "text-dim"
                }`}
                onClick={() => setProtocol(key)}
              >
                {label}
              </button>
            ))}
          </div>
          <label class="mb-1 block text-xs font-semibold">Model</label>
          <input class="input mono mb-3 max-w-sm" value={model} onInput={(e) => setModel((e.target as HTMLInputElement).value)} />
          <Snippet label={`cURL · ${path}`} code={curlSample(protocol, model.trim() || "automode")} />
        </div>
      </Card>

      <Card>
        <CardHead title="Use in your tools" sub="Point coding agents at this gateway with your own key." />
        <div class="px-[15px] py-3.5">
          <p class="mb-3 text-[11px] text-dim">
            <span class="mono">automode</span> is the gateway's virtual model, routed to the target set on the{" "}
            <a href="/config" onClick={(e) => { e.preventDefault(); navigate("config"); }}>Config</a> page. In Claude Code it plugs into auto-mode
            classification; your main model stays what the client picks.
          </p>
          <Snippet label="Claude Code · shell profile" code={claudeCodeEnv(origin)} />
          <Snippet
            label="Codex · ~/.codex/config.toml"
            code={`[model_providers.gnvipi]
name = "GNVipi gateway"
base_url = "${origin}/v1"
env_key = "GNVIPI_API_KEY"
wire_api = "responses"`}
          />
          <Snippet
            label="OpenCode · ~/.config/opencode/config.json"
            code={`{
  "provider": {
    "gnvipi": {
      "npm": "@ai-sdk/openai-compatible",
      "options": { "baseURL": "${origin}/v1", "apiKey": "{env:GNVIPI_API_KEY}" },
      "models": { "${model.trim() || "automode"}": {} }
    }
  }
}`}
          />
        </div>
      </Card>
    </>
  );
}