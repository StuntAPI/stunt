import { describe, expect, test } from "bun:test";
import { bootAdapter } from "../helpers";
import OpenAI from "openai";

// Drives openai-node (the official OpenAI TypeScript SDK) against the
// llm-style adapter. baseURL pins the client to the local adapter; the
// SDK's own typed serialization and SSE handling are exercised end to end.
describe("openai-node against llm-style", () => {
  test(
    "models list + chat completions round-trip",
    async () => {
      const h = await bootAdapter("llm-style");
      try {
        const openai = new OpenAI({
          apiKey: "sk-test-key",
          baseURL: h.base + "/v1",
          maxRetries: 0,
        });

        // ===== models.list returns the catalog =====
        const models = await openai.models.list();
        const ids = models.data.map((m) => m.id);
        expect(ids.length).toBeGreaterThan(0);
        expect(ids.every((id) => typeof id === "string" && id.length > 0)).toBe(true);

        // ===== chat.completions.create returns a typed echo response =====
        const chat = await openai.chat.completions.create({
          model: ids[0],
          messages: [{ role: "user", content: "hello from the official SDK" }],
        });
        expect(chat.object).toBe("chat.completion");
        expect(chat.choices.length).toBeGreaterThan(0);
        expect(chat.choices[0].message.role).toBe("assistant");
        expect(chat.choices[0].message.content).toBeTruthy();
        expect(chat.usage?.total_tokens).toBeGreaterThan(0);
      } finally {
        await h.stop();
      }
    },
    { timeout: 30_000 },
  );
});
