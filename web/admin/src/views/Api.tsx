import { useState } from "preact/hooks";
import { Card, CardHead, PageHeader } from "../components/ui";
import { ModelApiGuide, Snippet } from "../components/ApiGuide";
import { useGateway } from "../lib/context";
import { PROTOCOLS, guideOrigin } from "../lib/apiGuide";

export function ApiView() {
  const { navigate } = useGateway();
  const [model, setModel] = useState("automode");

  return (
    <>
      <PageHeader title="API" sub="Endpoints, sample code and agent setup" />

      <Card>
        <CardHead title="Base URL" sub="Every protocol below is served from this address." />
        <div class="px-[15px] py-3.5">
          <Snippet label="Base URL" code={guideOrigin} />
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
            Authenticate with a gateway API key from the <a href="/tokens" onClick={(e) => { e.preventDefault(); navigate("tokens"); }}>API Keys</a> page:
            <span class="mono"> Authorization: Bearer $GNVIPI_API_KEY</span> (or <span class="mono">x-api-key</span> for Anthropic clients).
          </p>
        </div>
      </Card>

      <Card>
        <CardHead title="Sample code" sub="Pick a protocol; the model id is editable." />
        <div class="space-y-3 px-[15px] py-3.5">
          <div>
            <label class="mb-1 block text-xs font-semibold">Model</label>
            <input class="input mono max-w-sm" value={model} onInput={(e) => setModel((e.target as HTMLInputElement).value)} />
          </div>
          <ModelApiGuide model={model} />
        </div>
      </Card>
    </>
  );
}