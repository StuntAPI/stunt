import { describe, expect, test } from "bun:test";
import { bootAdapter } from "../helpers";
import { WebClient } from "@slack/web-api";

// Drives @slack/web-api (Slack's official TypeScript SDK) against the
// slack-style adapter: the WebClient's own serialization + bearer auth +
// response parsing end to end. slackApiUrl pins every method call
// (chat.postMessage, ...) to the adapter's /api/ routes.
describe("@slack/web-api against slack-style", () => {
  test(
    "auth, channels, messages, reactions",
    async () => {
      const h = await bootAdapter("slack-style");
      try {
        const slack = new WebClient("xoxb-test-token", {
          slackApiUrl: h.base + "/api/",
          retryConfig: { retries: 0 },
        });

        // ===== auth.test validates the seeded bearer token =====
        const auth = await slack.auth.test();
        expect(auth.ok).toBe(true);
        expect(auth.user).toBeTruthy();

        // ===== conversations.create + list round-trip =====
        const conv = await slack.conversations.create({ name: "sdk-conf" });
        expect(conv.ok).toBe(true);
        const channelId = conv.channel?.id ?? "";
        expect(channelId).toBeTruthy();
        const list = await slack.conversations.list();
        expect(list.ok).toBe(true);
        expect((list.channels ?? []).some((c) => c.id === channelId)).toBe(true);

        // ===== chat.postMessage lands in conversations.history =====
        const posted = await slack.chat.postMessage({ channel: channelId, text: "SDK hello" });
        expect(posted.ok).toBe(true);
        expect(posted.ts).toBeTruthy();
        const history = await slack.conversations.history({ channel: channelId });
        expect(history.ok).toBe(true);
        expect((history.messages ?? []).some((m) => m.text === "SDK hello")).toBe(true);

        // ===== reactions.add on the posted message =====
        const react = await slack.reactions.add({
          channel: channelId,
          timestamp: posted.ts!,
          name: "thumbsup",
        });
        expect(react.ok).toBe(true);
      } finally {
        await h.stop();
      }
    },
    { timeout: 30_000 },
  );
});
