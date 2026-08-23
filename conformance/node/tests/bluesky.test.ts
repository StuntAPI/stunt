import { describe, expect, test } from "bun:test";
import { bootAdapter } from "../helpers";
import { AtpAgent } from "@atproto/api";

// Drives @atproto/api (the official Bluesky / AT Protocol TypeScript
// client) against the bluesky-style adapter. The AtpAgent is pointed at
// the booted service URL, so its XRPC serialization, session management
// (login + refresh) and record helpers all run stock.
describe("@atproto/api against bluesky-style", () => {
  test(
    "session, records, profile, search",
    async () => {
      const h = await bootAdapter("bluesky-style");
      try {
        const agent = new AtpAgent({ service: new URL(h.base) });

        // ===== login rides createSession and adopts did/handle/tokens =====
        const session = await agent.login({
          identifier: "conformance.test",
          password: "local-test-password",
        });
        expect(session.data.did).toMatch(/^did:plc:/);
        expect(session.data.handle).toBe("conformance.test");
        expect(session.data.accessJwt).toBeTruthy();
        expect(agent.session?.did).toBe(session.data.did);
        const did = session.data.did;

        // ===== agent.post creates an app.bsky.feed.post record =====
        const posted = await agent.post({
          text: "sdk conformance post",
          createdAt: new Date().toISOString(),
        });
        expect(posted.uri).toBe(`at://${did}/app.bsky.feed.post/` + posted.uri.split("/").pop());
        expect(posted.cid).toBeTruthy();

        // ===== searchPosts finds the created post by its text =====
        const found = await agent.api.app.bsky.feed.searchPosts({ q: "sdk conformance" });
        const uris = found.data.posts.map((p) => p.uri);
        expect(uris).toContain(posted.uri);

        // ===== getProfile resolves the session actor with counts =====
        const profile = await agent.getProfile({ actor: did });
        expect(profile.data.did).toBe(did);
        expect(profile.data.handle).toBe("conformance.test");
        expect(profile.data.postsCount).toBeGreaterThanOrEqual(1);

        // ===== resolveHandle is a public read (no session) =====
        const anon = new AtpAgent({ service: new URL(h.base) });
        const resolved = await anon.com.atproto.identity.resolveHandle({ handle: "conformance.test" });
        expect(resolved.data.did).toBe(did);

        // ===== unauthenticated record writes surface as XRPC 401s =====
        let unauthorized = false;
        try {
          await anon.com.atproto.repo.createRecord({
            repo: did,
            collection: "app.bsky.feed.post",
            record: { $type: "app.bsky.feed.post", text: "x", createdAt: new Date().toISOString() },
          });
        } catch (e: any) {
          unauthorized = true;
          expect(String(e.status ?? e.error ?? e)).toMatch(/401|AuthRequired/);
        }
        expect(unauthorized).toBe(true);

        // ===== deletePost removes the record from search =====
        await agent.deletePost(posted.uri);
        const after = await agent.api.app.bsky.feed.searchPosts({ q: "sdk conformance" });
        expect(after.data.posts.map((p) => p.uri)).not.toContain(posted.uri);

        // ===== refreshSession rotates the token pair via the SDK =====
        const oldAccess = agent.session?.accessJwt;
        const refreshed = await agent.sessionManager.refreshSession();
        expect(refreshed.data.accessJwt).toBeTruthy();
        expect(refreshed.data.accessJwt).not.toBe(oldAccess);
        // the rotated access token still authorizes writes
        const again = await agent.post({
          text: "post-refresh write",
          createdAt: new Date().toISOString(),
        });
        expect(again.uri).toContain(did);
      } finally {
        await h.stop();
      }
    },
    { timeout: 30_000 },
  );
});
