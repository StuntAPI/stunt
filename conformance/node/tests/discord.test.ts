import { describe, expect, test } from "bun:test";
import { bootAdapter } from "../helpers";
import { REST } from "@discordjs/rest";
import { Routes } from "discord-api-types/v10";

// Drives @discordjs/rest (the official Discord REST layer) against the
// discord-style adapter. `api` pins the REST instance to the local
// adapter; the SDK appends the /v10 version segment exactly like it does
// against the real API, which is why the adapter serves versioned routes.
describe("@discordjs/rest against discord-style", () => {
  test(
    "user, guild, channel + message lifecycle",
    async () => {
      const h = await bootAdapter("discord-style");
      try {
        const rest = new REST({ api: h.base, version: "10" }).setToken("mock-bot-token");

        // ===== users/@me resolves the bot user =====
        const me = (await rest.get(Routes.user("@me"))) as any;
        expect(me.id).toBeTruthy();
        expect(me.username).toBeTruthy();

        // ===== guilds resolve with channels =====
        const guilds = (await rest.get(Routes.userGuilds())) as any[];
        expect(guilds.length).toBeGreaterThan(0);
        const guildId = guilds[0].id;
        const guild = (await rest.get(Routes.guild(guildId))) as any;
        expect(guild.id).toBe(guildId);
        const channels = (await rest.get(Routes.guildChannels(guildId))) as any[];
        expect(channels.length).toBeGreaterThan(0);
        const channelId = channels.find((c) => c.type === 0)?.id ?? channels[0].id;

        // ===== message post lands in the channel history =====
        const posted = (await rest.post(Routes.channelMessages(channelId), {
          body: { content: "hello from @discordjs/rest" },
        })) as any;
        expect(posted.id).toBeTruthy();
        expect(posted.content).toBe("hello from @discordjs/rest");
        const history = (await rest.get(Routes.channelMessages(channelId))) as any[];
        expect(history.some((m) => m.id === posted.id)).toBe(true);

        // ===== single-message fetch round-trips =====
        const fetched = (await rest.get(
          Routes.channelMessage(channelId, posted.id),
        )) as any;
        expect(fetched.id).toBe(posted.id);
        expect(fetched.content).toBe("hello from @discordjs/rest");

        // ===== add-reaction is the real PUT and returns 204 =====
        // (the adapter models the endpoint as a documented no-op)
        await rest.put(
          Routes.channelMessageOwnReaction(channelId, posted.id, encodeURIComponent("👍")),
        );
      } finally {
        await h.stop();
      }
    },
    { timeout: 30_000 },
  );
});
