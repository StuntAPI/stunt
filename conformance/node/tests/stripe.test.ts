// stripe-node conformance: the seam proven by dogfooding (host/port/
// protocol — the SDK rejects a full apiBase key), typed creates,
// autoPagingEach walking has_more, and webhooks.constructEvent verifying
// the delivery HMAC + parsing data.object.
import { describe, expect, test } from "bun:test";
import Stripe from "stripe";
import { request } from "node:http";
import { bootAdapter } from "../helpers";

// rawPost performs a one-off form POST on a fresh connection
// (agent:false) — independent of any client library's connection state.
function rawPost(urlStr: string, body: string): Promise<number> {
  return new Promise((resolve, reject) => {
    const u = new URL(urlStr);
    const req = request(
      { hostname: u.hostname, port: u.port, path: u.pathname, method: "POST", agent: false },
      (res) => {
        res.resume();
        res.on("end", () => resolve(res.statusCode ?? 0));
      },
    );
    req.on("error", reject);
    req.setHeader("content-type", "application/x-www-form-urlencoded");
    req.setHeader("authorization", "Bearer sk_test_node_conformance");
    req.end(body);
  });
}

const SECRET = "whsec_stunt_mock_0123456789abcdef0123456789abcdef";

describe("stripe-node against stripe-style", () => {
  test(
    "full client + webhook lifecycle",
    async () => {
      const h = await bootAdapter("stripe-style");
      try {
        const u = new URL(h.base);
        const stripe = new Stripe("sk_test_node_conformance", {
          host: u.hostname,
          port: Number(u.port),
          protocol: "http",
          telemetry: false,
          maxNetworkRetries: 0,
        });

        // ===== Typed create/get (form+bracket encoded by the SDK) =====
        const cus = await stripe.customers.create({
          name: "Grace Hopper",
          email: "grace@synth.example",
          metadata: { source: "stripe-node-conformance" },
        });
        expect(cus.object).toBe("customer");
        expect(cus.name).toBe("Grace Hopper");
        const got = await stripe.customers.retrieve(cus.id);
        expect((got as Stripe.Customer).email).toBe("grace@synth.example");

        // ===== autoPagingEach walks has_more pages =====
        for (let i = 0; i < 3; i++) {
          await stripe.customers.create({ name: `paging ${i}` });
        }
        // Distinct ids pin BOTH failure modes: has_more always false
        // (stops early) and starting_after ignored (duplicates forever).
        const ids: string[] = [];
        let valved = false;
        await stripe.customers.list({ limit: 2 }).autoPagingEach((c: { id: string }) => {
          ids.push(c.id);
          if (ids.length > 10) {
            valved = true;
            return false;
          }
        });
        // The engine seeds a couple of customers, so the exact count is
        // not fixed — no-duplicates + no-valve pins both failure modes.
        expect(valved).toBe(false);
        expect(ids.length).toBeGreaterThanOrEqual(4);
        expect(new Set(ids).size).toBe(ids.length);

        // ===== Webhook registration, delivery verified by the SDK's own
        // constructEvent =====
        // NOTE: registration goes via a raw POST rather than
        // stripe.webhookEndpoints.create — under bun, stripe-node's fetch
        // layer intermittently drops the `url` form param when it names a
        // live same-process listener (the sink), sending only the other
        // params; reproduced with a fresh client against an echo server,
        // independent of stunt (plain bun fetch does not do this, and the
        // SDK's own encode step has the param). Everything after
        // registration — the trigger, the delivery, the verification —
        // stays SDK-driven.
        // With a fresh binary the SDK path may work — try it, fall back
        // to the raw POST if the url param vanishes again.
        let registeredViaSDK = false;
        try {
          const viaSDK = await stripe.webhookEndpoints.create({
            url: h.sinkUrl,
            enabled_events: ["payment_intent.succeeded"],
          });
          registeredViaSDK = viaSDK.id != null && viaSDK.url === h.sinkUrl;
        } catch {
          registeredViaSDK = false;
        }
        if (!registeredViaSDK) {
          console.warn("webhookEndpoints.create fell back to raw POST (bun/stripe-node url-param quirk?)");
        const regStatus = await rawPost(
          h.base + "/v1/webhook_endpoints",
          new URLSearchParams({
            url: h.sinkUrl,
            "enabled_events[0]": "payment_intent.succeeded",
          }).toString(),
        );
        expect(regStatus).toBeGreaterThanOrEqual(200);
        expect(regStatus).toBeLessThan(300);
        }

        const pm = await stripe.paymentMethods.create({
          type: "card",
          card: { token: "tok_visa" },
        });
        const pi = await stripe.paymentIntents.create({
          amount: 1234,
          currency: "usd",
          payment_method: pm.id,
          confirm: true,
        });
        expect(pi.status).toBe("succeeded");

        const d = await h.waitFor((x) => x.body.includes("payment_intent.succeeded"));
        const event = await stripe.webhooks.constructEventAsync(
          d.body,
          d.headers["stripe-signature"],
          SECRET,
        );
        expect(event.type).toBe("payment_intent.succeeded");
        expect((event.data.object as Record<string, unknown>).status).toBe("succeeded");

        // A tampered payload must FAIL the same verifier.
        const tampered = d.body.replace("succeeded", "tampered!");
        let threw = false;
        try {
          await stripe.webhooks.constructEventAsync(tampered, d.headers["stripe-signature"], SECRET);
        } catch {
          threw = true;
        }
        expect(threw).toBe(true);
      } finally {
        await h.stop();
      }
    },
    { timeout: 120_000 },
  );
});
