import { describe, expect, test } from "bun:test";
import { bootAdapter } from "../helpers";
import jsforce from "jsforce";

// Drives jsforce (the de-facto standard Salesforce JavaScript client)
// through its REAL login flow against the salesforce-style adapter: the
// password grant hits /services/oauth2/token, the returned instance_url
// (which the adapter echoes from the request host, like real Salesforce)
// becomes the API base, and sobject CRUD + SOQL ride the SDK's own
// serialization. version is pinned to the adapter's v60.0 surface.
describe("jsforce against salesforce-style", () => {
  test(
    "login, sobject CRUD + SOQL",
    async () => {
      const h = await bootAdapter("salesforce-style");
      try {
        // ===== SDK-driven login: with clientId+clientSecret configured,
        // jsforce's conn.login rides the OAuth2 password grant itself, and
        // adopts the adapter's echoed instance_url as its API base =====
        const conn = new jsforce.Connection({
          oauth2: { loginUrl: h.base, clientId: "sdk-conformance", clientSecret: "local-secret" },
          version: "60.0",
        });
        await conn.login("sdk@example.test", "local-test-password");
        expect(conn.accessToken).toBeTruthy();

        // ===== sobject create assigns an id =====
        const created = await conn.sobject("Account").create({ Name: "SDK Co" });
        expect(created.success).toBe(true);
        expect(created.id).toBeTruthy();

        // ===== retrieve round-trips the fields =====
        const got = await conn.sobject("Account").retrieve(created.id!);
        expect(got.Name).toBe("SDK Co");

        // ===== update patches and persists =====
        await conn.sobject("Account").update({ Id: created.id!, Name: "Renamed Co" });
        const after = await conn.sobject("Account").retrieve(created.id!);
        expect(after.Name).toBe("Renamed Co");

        // ===== SOQL through the SDK's query parser =====
        const found = await conn.query("SELECT Id, Name FROM Account WHERE Name = 'Renamed Co'");
        expect(found.totalSize).toBeGreaterThan(0);
        expect(found.records.some((r) => r.Id === created.id)).toBe(true);

        // ===== destroy removes the record =====
        await conn.sobject("Account").destroy(created.id!);
        const gone = await conn.query("SELECT Id FROM Account WHERE Id = '" + created.id + "'");
        expect(gone.totalSize).toBe(0);
      } finally {
        await h.stop();
      }
    },
    { timeout: 30_000 },
  );
});
