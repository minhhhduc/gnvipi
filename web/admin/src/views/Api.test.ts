import { describe, expect, it } from "vitest";
import { curlSample } from "./Api";

describe("curlSample", () => {
  it("sends OpenAI protocols to their paths with a bearer token", () => {
    const chat = curlSample("chat", "automode");
    expect(chat).toContain("/v1/chat/completions");
    expect(chat).toContain("Authorization: Bearer $GNVIPI_API_KEY");
    expect(chat).toContain('"model": "automode"');
    expect(curlSample("responses", "x")).toContain("/v1/responses");
  });

  it("uses x-api-key for Anthropic messages", () => {
    const messages = curlSample("messages", "automode");
    expect(messages).toContain("/v1/messages");
    expect(messages).toContain("x-api-key: $GNVIPI_API_KEY");
    expect(messages).not.toContain("Authorization");
  });
});