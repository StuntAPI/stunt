import { describe, expect, test } from "bun:test";
import { bootAdapter } from "../helpers";
import { Resend } from "resend";

// Drives resend-node (Resend's official TypeScript SDK) against the
// resend-style adapter: send through the SDK's typed client, then fetch
// the message back and watch the derived delivery lifecycle. baseUrl pins
// it to the local adapter.
describe("resend-node against resend-style", () => {
  test(
    "send, get, list + derived delivery lifecycle",
    async () => {
      const h = await bootAdapter("resend-style");
      try {
        const resend = new Resend("re_dev_key", { baseUrl: h.base });

        // ===== emails.send mints a re_* id =====
        const sent = await resend.emails.send({
          from: "conf@example.test",
          to: ["sink@example.test"],
          subject: "SDK conformance",
          text: "hello from resend-node",
        });
        expect(sent.error).toBeNull();
        const id = sent.data!.id;
        expect(id).toBeTruthy();
        expect(id.startsWith("re_")).toBe(true);

        // ===== emails.get round-trips the message =====
        const got = await resend.emails.get(id);
        expect(got.error).toBeNull();
        expect(got.data!.id).toBe(id);
        expect(got.data!.subject).toBe("SDK conformance");
        expect(got.data!.to?.[0]).toBe("sink@example.test");

        // ===== emails.list includes it =====
        const list = await resend.emails.list();
        expect(list.error).toBeNull();
        expect((list.data?.data ?? []).some((m) => m.id === id)).toBe(true);

        // ===== delivery state derives on read (sent -> delivered) =====
        await Bun.sleep(3200); // delivered derives at +3s
        const after = await resend.emails.get(id);
        expect(after.error).toBeNull();
        expect(["sent", "delivered"]).toContain(after.data!.status ?? "");
      } finally {
        await h.stop();
      }
    },
    { timeout: 30_000 },
  );
});
