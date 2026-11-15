import { describe, expect, it } from "vitest";
import { claudeCodeSettings, codexConfig, curlSample, openCodeConfig } from "../lib/apiGuide";

describe("curlSample", () => {
  it("sends OpenAI protocols to their paths with a bearer token", () => {
    const chat = curlSample("chat", "automode");
    expect(chat).toContain("/v1/chat/completions");
    expect(chat).toContain("Authorization: Bearer $GNVIPI_API_KEY");
    expect(chat).toContain('"model": "automode"');
    expect(curlSample("responses", "x")).toContain("/v1/responses");
  });

  it("puts automode in the gateway-wide Claude Code settings env", () => {
    const settings = JSON.parse(claudeCodeSettings("http://gw:8081"));
    expect(settings.env.CLAUDE_CODE_AUTO_MODE_MODEL).toBe("automode");
    expect(settings.env.CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY).toBe("1");
    expect(settings.env.ANTHROPIC_BASE_URL).toBe("http://gw:8081");
    expect(settings.env).not.toHaveProperty("ANTHROPIC_MODEL");
  });

  it("uses x-api-key for Anthropic messages", () => {
    const messages = curlSample("messages", "automode");
    expect(messages).toContain("/v1/messages");
    expect(messages).toContain("x-api-key: $GNVIPI_API_KEY");
    expect(messages).not.toContain("Authorization");
  });
});
describe("model snippets", () => {
  it("puts the model at the top level of the Codex config", () => {
    const codex = codexConfig("my/alias", "http://gw:8081");
    expect(codex.startsWith('model = "my/alias"')).toBe(true);
    expect(codex).toContain('model_provider = "gnvipi"');
    expect(codex).toContain('base_url = "http://gw:8081/v1"');
  });

  it("lists the model in the OpenCode provider map", () => {
    expect(openCodeConfig("my/alias", "http://gw:8081")).toContain('"my/alias": {}');
  });
});
