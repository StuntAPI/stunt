import { describe, expect, test } from "bun:test";
import { bootAdapter } from "../helpers";
import { SquareClient, SquareEnvironment } from "square";

// Drives the official square TypeScript SDK (v45 typed client) against
// the square-style adapter: OAuth token minting, then the payments and
// orders lifecycles through the SDK's own serialization (it sends the
// Square-Version header the adapter requires). baseUrl pins it locally.
describe("square-node against square-style", () => {
  test(
    "oauth, payments + orders lifecycle",
    async () => {
      const h = await bootAdapter("square-style");
      try {
        const mint = new SquareClient({
          environment: SquareEnvironment.Sandbox,
          baseUrl: h.base,
        });

        // ===== oAuth.obtainToken mints the bearer the adapter validates =====
        const tok = await mint.oAuth.obtainToken({
          grantType: "authorization_code",
          code: "sq0cgp-code123",
          clientId: "sq0idp-test",
          clientSecret: "shpss-test",
        });
        const token = tok.accessToken!;
        expect(token).toBeTruthy();
        expect(token.startsWith("EAAA")).toBe(true);

        const client = new SquareClient({
          token,
          environment: SquareEnvironment.Sandbox,
          baseUrl: h.base,
        });

        // ===== payments.create completes immediately for the ok nonce =====
        const pay = await client.payments.create({
          sourceId: "cnon:card-nonce-ok",
          idempotencyKey: "sdk-pay-1",
          amountMoney: { amount: 1500n, currency: "USD" },
        });
        expect(pay.payment?.id).toBeTruthy();
        expect(pay.payment!.status).toBe("COMPLETED");
        const got = await client.payments.get({ paymentId: pay.payment!.id! });
        expect(got.payment!.id).toBe(pay.payment!.id);
        expect(got.payment!.amountMoney?.amount).toBe(1500n);

        // ===== orders.create prices line items and mints an order id =====
        const order = await client.orders.create({
          order: {
            locationId: "L-MOCK",
            lineItems: [
              {
                name: "SDK widget",
                quantity: "2",
                basePriceMoney: { amount: 499n, currency: "USD" },
              },
            ],
          },
          idempotencyKey: "sdk-order-1",
        });
        expect(order.order?.id).toBeTruthy();
        expect(order.order!.state).toBe("OPEN");
        expect(order.order!.totalMoney?.amount).toBe(998n);

        // ===== orders.get round-trips the priced order =====
        const fetched = await client.orders.get({ orderId: order.order!.id! });
        expect(fetched.order!.id).toBe(order.order!.id);
        expect(fetched.order!.lineItems?.[0]?.name).toBe("SDK widget");
      } finally {
        await h.stop();
      }
    },
    { timeout: 30_000 },
  );
});
