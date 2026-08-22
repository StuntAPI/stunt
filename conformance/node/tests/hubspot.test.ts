import { describe, expect, test } from "bun:test";
import { bootAdapter } from "../helpers";
import { Client } from "@hubspot/api-client";

// Drives @hubspot/api-client (HubSpot's official TypeScript SDK) against
// the hubspot-style adapter: CRM object CRUD + search through the SDK's
// own typed client. basePath pins it to the local adapter.
describe("@hubspot/api-client against hubspot-style", () => {
  test(
    "company CRUD + search through the typed client",
    async () => {
      const h = await bootAdapter("hubspot-style");
      try {
        const hubspot = new Client({
          accessToken: "pat-mock-token",
          basePath: h.base,
        });

        // ===== basicApi.create assigns an id and echoes properties =====
        const created = await hubspot.crm.companies.basicApi.create({
          properties: { name: "Conformance Co", domain: "conf.test" },
        });
        expect(created.id).toBeTruthy();
        expect(created.properties.name).toBe("Conformance Co");

        // ===== basicApi.getById round-trips =====
        const got = await hubspot.crm.companies.basicApi.getById(created.id);
        expect(got.id).toBe(created.id);
        expect(got.properties.domain).toBe("conf.test");

        // ===== basicApi.update patches a property =====
        const updated = await hubspot.crm.companies.basicApi.update(created.id, {
          properties: { name: "Renamed Co" },
        });
        expect(updated.properties.name).toBe("Renamed Co");

        // ===== searchApi.doSearch finds the record by name =====
        const found = await hubspot.crm.companies.searchApi.doSearch({
          filterGroups: [
            {
              filters: [
                { propertyName: "name", operator: "EQ", value: "Renamed Co" },
              ],
            },
          ],
          sorts: [],
          limit: 10,
          after: 0,
          properties: [],
        });
        expect(found.results.some((r) => r.id === created.id)).toBe(true);

        // ===== archive hides the record from reads =====
        await hubspot.crm.companies.basicApi.archive(created.id);
        const page = await hubspot.crm.companies.basicApi.getPage();
        expect(page.results.some((r) => r.id === created.id)).toBe(false);
        let gone = false;
        try {
          await hubspot.crm.companies.basicApi.getById(created.id);
        } catch {
          gone = true; // the SDK throws on the adapter's 404
        }
        expect(gone).toBe(true);
      } finally {
        await h.stop();
      }
    },
    { timeout: 30_000 },
  );
});
