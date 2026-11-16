import { useState } from "preact/hooks";
import { useUi } from "./ui";
import { useGateway, useGatewayOrigin } from "../lib/context";
import { PROTOCOLS, claudeCodeSettings, codexConfig, curlSample, openCodeConfig, type Protocol } from "../lib/apiGuide";

export function Snippet({ label, code }: { label: string; code: string }) {
  const { toast } = useUi();
  const copy = () => {
    navigator.clipboard.writeText(code).then(
      () => toast("Copied"),
      () => toast("Copy failed", "bad"),
    );
  };
  return (
    <div>
      <div class="mb-1 flex items-center justify-between">
        <span class="text-[11px] font-semibold uppercase tracking-[0.05em] text-dim">{label}</span>
        <button type="button" class="btn btn-sm" onClick={copy}>Copy</button>
      </div>
      <pre class="mono overflow-x-auto rounded-lg border border-line bg-panel2 px-3 py-2.5 text-[11px] leading-5">{code}</pre>
    </div>
  );
}

/** Where automode is configured, gateway side and agent side. */
export function AutomodeNote({ protocol }: { protocol: Protocol }) {
  const { navigate } = useGateway();
  return (
    <div class="rounded-lg border border-line bg-panel2 px-3 py-2.5 text-[11px] leading-5 text-dim">
      <div class="mb-1 text-xs font-semibold text-fg">Setting up automode</div>
      <div>
        1. Gateway target: open{" "}
        <a href="/config" onClick={(e) => { e.preventDefault(); navigate("config"); }}>Config</a>{" "}
        and choose the <span class="font-semibold text-fg">Auto model</span> that <span class="mono">model: "automode"</span> is routed to. Each account has its own target.
      </div>
      {protocol === "messages" && (
        <div>
          2. Claude Code: add the <span class="mono">env</span> block below to <span class="mono">~/.claude/settings.json</span> — it is gateway-wide and applies to every project.
          The client only holds the <span class="font-semibold text-fg">gateway token</span> (<span class="mono">$GNVIPI_GATEWAY_TOKEN</span>, from API Keys);
          provider Auth key / API key stay on the server in the provider's keys. Leave <span class="mono">ANTHROPIC_API_KEY</span> empty.
        </div>
      )}
      <div>
        {protocol === "messages" ? "3." : "2."} Built-in models: you can set <span class="mono">{protocol === "messages" ? "$your_model" : "model"}</span> to <span class="mono">automode</span> (routes to your configured Auto model) or <span class="mono">cycle</span> (rotates across enabled models).
      </div>
    </div>
  );
}

/** Protocol-tabbed cURL plus the agent setup snippets for one model alias. */
export function ModelApiGuide({ model }: { model: string }) {
  const origin = useGatewayOrigin();
  const [protocol, setProtocol] = useState<Protocol>("chat");
  const path = PROTOCOLS.find(([key]) => key === protocol)?.[2] ?? "";
  const alias = model.trim() || "automode";
  return (
    <div class="space-y-3">
      <div class="grid grid-cols-3 gap-1 rounded-xl border border-line bg-panel2 p-1">
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
      <Snippet label={`cURL · ${path}`} code={curlSample(protocol, alias, origin)} />
      <AutomodeNote protocol={protocol} />
      {protocol === "messages" && (
        <Snippet label="Claude Code · ~/.claude/settings.json" code={claudeCodeSettings(origin)} />
      )}
      {protocol === "responses" && (
        <Snippet label="Codex · ~/.codex/config.toml" code={codexConfig(alias, origin)} />
      )}
      {protocol === "chat" && (
        <Snippet label="OpenCode · ~/.config/opencode/config.json" code={openCodeConfig(alias, origin)} />
      )}
    </div>
  );
}