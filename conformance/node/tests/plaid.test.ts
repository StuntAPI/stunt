import { describe, expect, test } from "bun:test";
import { bootAdapter } from "../helpers";
import { Configuration, PlaidApi } from "plaid";

// Drives plaid-node (Plaid's official TypeScript SDK) against the
// plaid-style adapter. The SDK sends PLAID-CLIENT-ID / PLAID-SECRET
// headers and its own axios serialization; Configuration.basePath pins it
// to the local adapter.
describe("plaid-node against plaid-style", () => {
  test(
    "link token, item exchange, balances, transactions sync",
    async () => {
      const h = await bootAdapter("plaid-style");
      try {
        const client = new PlaidApi(
          new Configuration({
            basePath: h.base,
            apiKey: "conf-mock",
          }),
        );

        // ===== link/token/create mints a session =====
        const link = await client.linkTokenCreate({
          client_name: "conformance",
          products: ["transactions"],
          country_codes: ["US"],
          language: "en",
          user: { client_user_id: "conf-user" },
        } as any);
        expect(link.data.link_token).toBeTruthy();
        expect(link.data.expiration).toBeTruthy();

        // ===== sandbox public_token -> exchange -> item with accounts =====
        const pt = await client.sandboxPublicTokenCreate({
          institution_id: "ins_1",
          initial_products: ["transactions"],
        } as any);
        expect(pt.data.public_token).toBeTruthy();
        const ex = await client.itemPublicTokenExchange({
          public_token: pt.data.public_token,
        });
        const accessToken = ex.data.access_token;
        expect(accessToken).toBeTruthy();
        expect(ex.data.item_id).toBeTruthy();

        // ===== accounts/balance/get over the SDK's typed response =====
        const accts = await client.accountsBalanceGet({ access_token: accessToken } as any);
        expect(accts.data.accounts.length).toBeGreaterThan(0);
        const acct = accts.data.accounts[0];
        expect(acct.account_id).toBeTruthy();
        expect(acct.balances).toBeTruthy();

        // ===== transactions/sync cursor round-trip =====
        const sync = await client.transactionsSync({ access_token: accessToken } as any);
        expect(sync.data.next_cursor).toBeTruthy();
        expect(sync.data.added.length).toBeGreaterThan(0);
      } finally {
        await h.stop();
      }
    },
    { timeout: 30_000 },
  );
});
