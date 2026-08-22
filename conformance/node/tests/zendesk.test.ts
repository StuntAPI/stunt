import { describe, expect, test } from "bun:test";
import { bootAdapter } from "../helpers";
import { createClient } from "node-zendesk";

// Drives node-zendesk (the de-facto standard Zendesk JavaScript client)
// against the zendesk-style adapter. endpointUri pins it locally; the
// client's Basic auth (email/token:secret) and its canonical .json paths
// (which the adapter serves as real Zendesk does) are exercised as-is.
describe("node-zendesk against zendesk-style", () => {
  test(
    "ticket lifecycle + search",
    async () => {
      const h = await bootAdapter("zendesk-style");
      try {
        const zd = createClient({
          endpointUri: h.base + "/api/v2",
          // the client itself appends the "/token:" suffix to the username
          username: "admin@example.com",
          token: "test-secret",
        });

        // node-zendesk wraps every call as { response, result }
        const R = (p: any) => p?.result ?? p;

        // ===== create assigns a numeric id =====
        const t = R(await zd.tickets.create({
          subject: "SDK conformance ticket",
          comment: { body: "hello from node-zendesk" },
          requester: { name: "SDK Agent", email: "agent@example.test" },
          type: "incident",
        }));
        expect(t.id).toBeTruthy();

        // ===== show round-trips; status update persists =====
        const got = R(await zd.tickets.show(t.id));
        expect(got.subject).toBe("SDK conformance ticket");
        const updated = R(await zd.tickets.update(t.id, { status: "solved" }));
        expect(updated.status).toBe("solved");

        // ===== comment created at ticket-create time is readable =====
        // (node-zendesk v6 ships no comment-write method)
        const comments = R(await (zd as any).tickets.getComments(t.id));
        expect(comments.length).toBeGreaterThan(0);

        // ===== list includes the ticket =====
        const listed = R(await zd.tickets.list());
        expect(listed.some((x: any) => x.id === t.id)).toBe(true);

        // ===== substring search finds it by subject =====
        // (the adapter's documented search is substring-only)
        const found = R(await zd.search.query("SDK conformance"));
        const hits = Array.isArray(found) ? found : (found.results ?? []);
        expect(hits.some((r: any) => r.id === t.id)).toBe(true);

        // ===== delete removes it =====
        await zd.tickets.delete(t.id!);
        let gone = false;
        try {
          await zd.tickets.show(t.id!);
        } catch {
          gone = true;
        }
        expect(gone).toBe(true);
      } finally {
        await h.stop();
      }
    },
    { timeout: 30_000 },
  );
});
