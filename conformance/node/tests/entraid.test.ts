import { describe, expect, test } from "bun:test";
import { bootAdapter } from "../helpers";
import {
  Client,
  Context,
  GraphError,
  Middleware,
  MiddlewareFactory,
  PageIterator,
} from "@microsoft/microsoft-graph-client";

// Drives @microsoft/microsoft-graph-client (the Microsoft Graph JavaScript
// client) against the entra-id-style adapter.
//
// Construction note: v3 ClientOptions has baseUrl/customHosts, but the stock
// AuthenticationHandler only attaches the Bearer header when the request URL
// is https:// on a known Graph host or a customHosts entry
// (GraphRequestUtil.isValidEndpoint is https-only), so baseUrl cannot point at
// a local http mock — and on non-Graph URLs the handler actively DELETES any
// preset Authorization header. The client therefore keeps the SDK's stock
// graph.microsoft.com base (auth engages) and one spliced middleware rebases
// each authenticated request onto the booted adapter — the SDK's documented
// custom-middleware seam. The token itself comes from walking the adapter's
// own authorize+token endpoints (Microsoft identity platform v2.0 flow).
describe("@microsoft/microsoft-graph-client against entra-id-style", () => {
  test(
    "oauth code flow, graph reads/writes, error decoding",
    async () => {
      const h = await bootAdapter("entra-id-style");
      try {
        // --- adapter-side OAuth2 (walked like a browser + backend) ---
        const redirectUri = "http://localhost:8931/callback";
        const state = "entraid-suite-state";
        const clientId = "sdk-conformance-client";
        const clientSecret = "sdk-conformance-secret";
        const scope = "openid profile User.Read offline_access";

        // ===== authorize redirects with code + state =====
        const authorize = await fetch(
          h.base +
            "/common/oauth2/v2.0/authorize?" +
            new URLSearchParams({
              client_id: clientId,
              redirect_uri: redirectUri,
              response_type: "code",
              state,
              scope,
              prompt: "admin_consent",
            }),
          { redirect: "manual" },
        );
        expect(authorize.status).toBe(302);
        const back = new URL(authorize.headers.get("location")!, redirectUri);
        expect(back.searchParams.get("state")).toBe(state);
        expect(back.searchParams.get("session_state")).toBeTruthy();
        const code = back.searchParams.get("code")!;
        expect(code).toBeTruthy();

        // ===== token exchange mints a JWKS-verifiable RS256 bearer =====
        const tokenRes = await fetch(h.base + "/common/oauth2/v2.0/token", {
          method: "POST",
          body: new URLSearchParams({
            grant_type: "authorization_code",
            code,
            client_id: clientId,
            client_secret: clientSecret,
            redirect_uri: redirectUri,
            scope,
          }),
        });
        expect(tokenRes.status).toBe(200);
        const tokens: any = await tokenRes.json();
        expect(tokens.token_type).toBe("Bearer");
        expect(tokens.expires_in).toBe(3599);
        expect(tokens.ext_expires_in).toBe(3599);
        expect(tokens.refresh_token).toBeTruthy();
        const accessToken: string = tokens.access_token;

        // The access token is a real RS256 JWT verifiable against the JWKS.
        const jwks: any = await (
          await fetch(h.base + "/common/discovery/v2.0/keys")
        ).json();
        const jwk = jwks.keys[0];
        expect(jwk.kty).toBe("RSA");
        expect(jwk.alg).toBe("RS256");
        expect(jwk.use).toBe("sig");
        const [jh, jp, js] = accessToken.split(".");
        const b64urlToJson = (s: string) =>
          JSON.parse(atob(s.replace(/-/g, "+").replace(/_/g, "/")));
        const jwtHeader = b64urlToJson(jh);
        expect(jwtHeader.alg).toBe("RS256");
        expect(jwtHeader.kid).toBe(jwk.kid);
        const claims = b64urlToJson(jp);
        expect(claims.aud).toBe(clientId);
        expect(claims.iss).toBe("https://login.microsoftonline.com/mock-tenant/v2.0");
        expect(claims.scp).toContain("User.Read");
        const b64urlToBytes = (s: string) =>
          Uint8Array.from(
            atob(s.replace(/-/g, "+").replace(/_/g, "/")),
            (c) => c.charCodeAt(0),
          );
        const verifyKey = await crypto.subtle.importKey(
          "jwk",
          jwk,
          { name: "RSASSA-PKCS1-v1_5", hash: "SHA-256" },
          false,
          ["verify"],
        );
        const signatureOk = await crypto.subtle.verify(
          "RSASSA-PKCS1-v1_5",
          verifyKey,
          b64urlToBytes(js),
          new TextEncoder().encode(`${jh}.${jp}`),
        );
        expect(signatureOk).toBe(true);

        // ===== authProvider feeds the SDK; /v1.0/me resolves the token's user =====
        const rebase = (base: string): Middleware => {
          const mw = {
            next: undefined as Middleware | undefined,
            async execute(ctx: Context) {
              // Rebase the authenticated graph.microsoft.com URL onto the
              // booted adapter (the SDK's AuthenticationHandler has already
              // run and attached the Bearer header).
              const url = new URL(
                typeof ctx.request === "string" ? ctx.request : ctx.request.url,
              );
              ctx.request = base + url.pathname + url.search;
              await mw.next!.execute(ctx);
            },
            setNext(next: Middleware) {
              mw.next = next;
            },
          };
          return mw;
        };
        let providerCalls = 0;
        const graphFor = (token: () => Promise<string>) => {
          const chain = MiddlewareFactory.getDefaultMiddlewareChain({
            getAccessToken: async () => {
              providerCalls++;
              return await token();
            },
          });
          chain.splice(1, 0, rebase(h.base));
          return Client.initWithMiddleware({ middleware: chain });
        };
        const graph = graphFor(async () => accessToken);

        const me: any = await graph.api("/me").get();
        expect(me.id).toBe(claims.sub);
        expect(me.userPrincipalName).toContain("@mock-tenant.onmicrosoft.com");
        expect(me["@odata.context"]).toContain("$metadata#users");
        expect(providerCalls).toBeGreaterThan(0); // the SDK consulted the provider

        // ===== users create + get round-trip (by id and by UPN) =====
        const upn = "sdk-conf-user@mock-tenant.onmicrosoft.com";
        const created: any = await graph.api("/users").post({
          accountEnabled: true,
          displayName: "SDK Conformance User",
          mailNickname: "sdk-conf-user",
          userPrincipalName: upn,
        });
        expect(created.id).toBeTruthy();
        expect(created.userPrincipalName).toBe(upn);
        const got: any = await graph.api(`/users/${created.id}`).get();
        expect(got.id).toBe(created.id);
        expect(got.displayName).toBe("SDK Conformance User");
        expect(got.userPrincipalName).toBe(upn);
        expect(got.accountEnabled).toBe(true);
        const byUpn: any = await graph.api(`/users/${upn}`).get();
        expect(byUpn.id).toBe(created.id);

        // ===== users list pages via $top + $skipToken + PageIterator =====
        // A second user so the $top=1 walk spans at least three pages.
        await graph.api("/users").post({
          displayName: "SDK Paging User",
          userPrincipalName: "sdk-paging-user@mock-tenant.onmicrosoft.com",
        });
        const page1: any = await graph.api("/users").top(1).get();
        expect(page1.value).toHaveLength(1);
        const nextLink: string | undefined = page1["@odata.nextLink"];
        expect(nextLink).toBeTruthy();
        expect(nextLink!.startsWith("https://graph.microsoft.com/v1.0/users?")).toBe(
          true,
        );
        const skipToken = new URL(nextLink!).searchParams.get("$skipToken")!;
        const page2: any = await graph.api("/users").top(1).skipToken(skipToken).get();
        expect(page2.value).toHaveLength(1);
        expect(page2.value[0].id).not.toBe(page1.value[0].id);
        const all: any = await graph.api("/users").get();
        const ids = all.value.map((u: any) => u.id);
        expect(ids).toContain(me.id); // the OAuth-minted user is listed
        expect(ids).toContain(created.id); // so is the created one
        expect(ids).toContain(page1.value[0].id);
        expect(ids).toContain(page2.value[0].id);
        // The SDK's own PageIterator walks the absolute @odata.nextLink chain.
        const seen: any[] = [];
        const pageIterator = new PageIterator(graph, page1, (u: any) => {
          seen.push(u);
          return true;
        });
        await pageIterator.iterate();
        expect(seen.map((u: any) => u.id).sort()).toEqual([...ids].sort());

        // ===== applications + servicePrincipals reads =====
        const apps: any = await graph.api("/applications").get();
        expect(apps.value.length).toBeGreaterThanOrEqual(2);
        for (const a of apps.value) {
          expect(a.appId).toMatch(/^[0-9a-f-]{36}$/);
          expect(a.displayName).toBeTruthy();
          expect(a.createdDateTime).toBeTruthy();
        }
        const sps: any = await graph.api("/servicePrincipals").get();
        expect(sps.value.length).toBeGreaterThanOrEqual(2);
        const graphSp = sps.value.find(
          (s: any) => s.servicePrincipalType === "FirstParty",
        );
        expect(graphSp).toBeTruthy();
        expect(graphSp!.appRoles.length).toBeGreaterThan(0);

        // ===== refresh grant rotates the token the SDK presents =====
        const refreshRes = await fetch(h.base + "/common/oauth2/v2.0/token", {
          method: "POST",
          body: new URLSearchParams({
            grant_type: "refresh_token",
            refresh_token: tokens.refresh_token,
            client_id: clientId,
            client_secret: clientSecret,
            scope,
          }),
        });
        expect(refreshRes.status).toBe(200);
        const refreshed: any = await refreshRes.json();
        expect(refreshed.access_token).toBeTruthy();
        expect(refreshed.access_token).not.toBe(accessToken);
        const graphRefreshed = graphFor(async () => refreshed.access_token);
        const meAgain: any = await graphRefreshed.api("/me").get();
        expect(meAgain.id).toBe(me.id); // same user, new token

        // ===== error decoding: 404 and 401 surface as GraphError =====
        let err: any;
        try {
          await graph.api("/users/00000000-0000-0000-0000-ffffffffffff").get();
        } catch (e) {
          err = e;
        }
        expect(err).toBeInstanceOf(GraphError);
        expect(err.statusCode).toBe(404);
        expect(err.code).toBe("Request_ResourceNotFound");
        expect(err.message).toContain("does not exist");

        const badGraph = graphFor(async () => "not.a.jwt");
        let authErr: any;
        try {
          await badGraph.api("/me").get();
        } catch (e) {
          authErr = e;
        }
        expect(authErr).toBeInstanceOf(GraphError);
        expect(authErr.statusCode).toBe(401);
        expect(authErr.code).toBe("InvalidAuthenticationToken");
      } finally {
        await h.stop();
      }
    },
    { timeout: 30_000 },
  );
});
