// Node-side conformance harness: boots the REAL stunt binary (`stunt up`)
// against a generated manifest, waits for the runtime file, and exposes the
// service base URL plus a webhook sink. This doubles as an end-to-end test
// of the CLI itself — the Go suites use the engine directly; these suites
// go through the actual product entry point.
import { mkdtempSync, writeFileSync, readFileSync, existsSync, rmSync } from "node:fs";
import { join, resolve } from "node:path";
import { tmpdir } from "node:os";

export type Delivery = { url: string; body: string; headers: Record<string, string> };

export type Harness = {
  base: string;
  sinkUrl: string;
  deliveries: () => Delivery[];
  waitFor: (pred: (d: Delivery) => boolean, ms?: number) => Promise<Delivery>;
  stop: () => Promise<void>;
};

const STUNT_BIN = process.env.STUNT_BIN ?? "stunt";
const repoRoot = resolve(import.meta.dir, "..", "..");

export async function bootAdapter(adapter: string): Promise<Harness> {
  // The sink records every delivery — body, headers, and URL (the
  // signature formulas MAC the URL too).
  const deliveries: Delivery[] = [];
  const sink = Bun.serve({
    port: 0,
    async fetch(req) {
      const u = new URL(req.url);
      const headers: Record<string, string> = {};
      req.headers.forEach((v, k) => (headers[k] = v));
      deliveries.push({
        url: `http://${req.headers.get("host")}${u.pathname}${u.search}`,
        body: req.method === "POST" ? await req.text() : "",
        headers,
      });
      return new Response(null, { status: 200 });
    },
  });

  const dir = mkdtempSync(join(tmpdir(), "stunt-node-conf-"));
  // Port mode requires a concrete base_port (0 is rejected by manifest
  // validation — the Go suites use the engine directly and bypass it).
  // Probe-bind a free one; the socket stays open until just before the
  // spawn to keep the race window small.
  const probe = Bun.listen({ hostname: "127.0.0.1", port: 0, socket: { data() {} } });
  const basePort = probe.port;
  writeFileSync(
    join(dir, "stunt.yaml"),
    `version: 1
network:
  mode: port
  base_port: ${basePort}
services:
  svc:
    adapter: ${join(repoRoot, "adapters", adapter)}
    config:
      webhook_url: ${sink.url}
`,
  );

  const errBuf: string[] = [];
  const proc = Bun.spawn([STUNT_BIN, "up", "--manifest", join(dir, "stunt.yaml")], {
    stdout: "ignore",
    stderr: "pipe",
  });
  (async () => {
    const t = await new Response(proc.stderr).text();
    errBuf.push(t);
  })();
  probe.stop(true);

  const cleanupFail = (msg: string): never => {
    proc.kill();
    sink.stop(true);
    rmSync(dir, { recursive: true, force: true });
    const tail = errBuf.join("").trim().split("\n").slice(-6).join("\n");
    throw new Error(tail ? `${msg}\nstunt up stderr tail:\n${tail}` : msg);
  };

  // Poll the runtime file for the assigned address.
  const runtimePath = join(dir, ".stunt", "runtime", "up.json");
  const deadline = Date.now() + 20_000;
  let base = "";
  while (Date.now() < deadline) {
    if (existsSync(runtimePath)) {
      try {
        const rt = JSON.parse(readFileSync(runtimePath, "utf8"));
        if (rt.addresses?.length > 0) {
          base = rt.addresses[0];
          break;
        }
      } catch {}
    }
    await Bun.sleep(100);
  }
  if (!base) {
    cleanupFail(`stunt up did not write ${runtimePath} within 20s`);
  }

  // Readiness: any HTTP answer means the listener is live. Bounded per
  // attempt; a boot that never answers is a failure, not silence.
  for (let i = 0; i < 100; i++) {
    try {
      await fetch(base + "/", { signal: AbortSignal.timeout(500) });
      break;
    } catch {
      if (i === 99) cleanupFail(`adapter never answered on ${base}`);
      await Bun.sleep(100);
    }
  }

  return {
    base,
    sinkUrl: sink.url,
    deliveries: () => [...deliveries],
    async waitFor(pred, ms = 10_000) {
      const end = Date.now() + ms;
      while (Date.now() < end) {
        const hit = deliveries.find(pred);
        if (hit) return hit;
        await Bun.sleep(100);
      }
      throw new Error(`no matching delivery arrived within ${ms}ms (${deliveries.length} seen)`);
    },
    async stop() {
      proc.kill();
      await proc.exited;
      sink.stop(true);
      rmSync(dir, { recursive: true, force: true });
    },
  };
}

export function expect(cond: unknown, msg: string): asserts cond {
  if (!cond) throw new Error(msg);
}
