// twilio-node conformance: a custom httpClient (the SDK's documented
// injection point) rewrites scheme+host to the adapter — Twilio signs no
// request URLs, so this is sound. Lifecycle via SDK fetches, and callback
// verification through twilio-node's own validateRequest.
import { describe, expect, test } from "bun:test";
import twilio from "twilio";
import { bootAdapter, type Harness } from "../helpers";

const SID = "AC0123456789abcdef0123456789abcdef".replace("AC0", "AC" + "0");
const AUTH_TOKEN = "feed0000face1111beef2222cafe3333";

// FetchBackedClient implements the RequestClient contract
// ({statusCode, body, headers}) against the stunt base.
class FetchBackedClient {
  constructor(private base: string) {}
  async request(opts: Record<string, any>) {
    const u = new URL(opts.uri);
    const url = new URL(u.pathname + u.search, this.base);
    if (opts.params) {
      for (const [k, v] of Object.entries(opts.params)) url.searchParams.set(k, String(v));
    }
    const headers: Record<string, string> = {};
    for (const [k, v] of Object.entries(opts.headers ?? {})) if (v != null) headers[k] = String(v);
    if (opts.username && opts.password) {
      headers.Authorization =
        "Basic " + Buffer.from(`${opts.username}:${opts.password}`).toString("base64");
    }
    let body: string | undefined;
    const method = String(opts.method ?? "get").toUpperCase();
    if (opts.data && headers["Content-Type"] === "application/x-www-form-urlencoded") {
      body = new URLSearchParams(
        Object.entries(opts.data).map(([k, v]) => [k, String(v)]),
      ).toString();
    } else if (opts.data != null) {
      body = JSON.stringify(opts.data);
    }
    const res = await fetch(url, { method, headers, body });
    const text = await res.text();
    let parsed: unknown = text;
    try {
      parsed = JSON.parse(text);
    } catch {}
    const outHeaders: Record<string, string> = {};
    res.headers.forEach((v, k) => (outHeaders[k] = v));
    return { statusCode: res.status, body: parsed, headers: outHeaders };
  }
}

async function pollStatus(h: Harness, sid: string, want: string[]): Promise<string> {
  const deadline = Date.now() + 10_000;
  let status = "";
  while (Date.now() < deadline) {
    const res = await fetch(`${h.base}/2010-04-01/Accounts/${SID}/Messages/${sid}.json`, {
      headers: { Authorization: "Basic " + Buffer.from(`${SID}:${AUTH_TOKEN}`).toString("base64") },
    });
    const msg = (await res.json()) as Record<string, any>;
    if (want.includes(msg.status)) {
      status = msg.status;
      break;
    }
    await Bun.sleep(300);
  }
  return status;
}

describe("twilio-node against twilio-style", () => {
  test(
    "lifecycle + signed callbacks via validateRequest",
    async () => {
      const h = await bootAdapter("twilio-style");
      try {
        const client = twilio(SID, AUTH_TOKEN, {
          httpClient: new FetchBackedClient(h.base) as any,
          lazyLoading: false,
        });

        // ===== Create + SDK fetches drive the lifecycle =====
        const msg = await client.api.v2010.accounts(SID).messages.create({
          to: "+15550002222",
          from: "+15550001111",
          body: "node conformance hello",
        });
        expect(msg.sid).toMatch(/^SM/);

        // Poll via the SDK until terminal (drives derive-on-read
        // transitions, which fire the callbacks).
        const deadline = Date.now() + 10_000;
        let status = "";
        // Drive to DELIVERED (not just sent): the delivered callback only
        // fires on the read that derives the second transition.
        while (Date.now() < deadline) {
          const m = await client.api.v2010.accounts(SID).messages(msg.sid).fetch();
          if (["delivered", "failed"].includes(m.status)) {
            status = m.status;
            break;
          }
          await Bun.sleep(300);
        }
        expect(status).toBe("delivered");

        // ===== Callbacks verified by twilio-node's own validateRequest =====
        const first = await h.waitFor((d) => d.headers["x-twilio-signature"] != null);
        const params = Object.fromEntries(new URLSearchParams(first.body));
        const url = first.url;
        const ok = twilio.validateRequest(AUTH_TOKEN, first.headers["x-twilio-signature"], url, params);
        expect(ok).toBe(true);

        // The terminal (delivered) callback MUST arrive — the loop only
        // verifies what's present, so a dropped unsigned second callback
        // must fail here, not slip through.
        await h.waitFor((d) => d !== first && d.headers["x-twilio-signature"] != null, 8_000);
        for (const d of h.deliveries()) {
          if (!d.headers["x-twilio-signature"]) continue;
          const p = Object.fromEntries(new URLSearchParams(d.body));
          expect(twilio.validateRequest(AUTH_TOKEN, d.headers["x-twilio-signature"], d.url, p)).toBe(
            true,
          );
        }

        // ===== The magic invalid number -> failed =====
        const bad = await client.api.v2010.accounts(SID).messages.create({
          to: "+15005550001",
          from: "+15550001111",
          body: "should fail",
        });
        const badStatus = await pollStatus(h, bad.sid, ["failed", "sent"]);
        expect(badStatus).toBe("failed");
      } finally {
        await h.stop();
      }
    },
    { timeout: 120_000 },
  );
});
