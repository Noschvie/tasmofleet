import { ChildProcess, spawn, execFileSync } from "node:child_process";
import { createServer, request as httpRequest, IncomingMessage, ServerResponse, Server } from "node:http";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { request as httpsRequest } from "node:https";
import { createInterface } from "node:readline";
import { AddressInfo, createServer as netServer } from "node:net";

export const root = resolve(__dirname, "../..");
// E2E_BIN_DIR lets a run use binaries built from another commit (before/after comparisons).
const binDir = process.env.E2E_BIN_DIR ?? join(root, "bin");
export const binary = join(binDir, "tasmofleet");
export const devenvBinary = join(binDir, "tasmofleet-devenv");

// Ports come from a block per Playwright worker below the kernel's ephemeral range
// (32768+). Asking the kernel for "port 0" and closing the socket again is racy: the
// kernel hands the same port to concurrent callers (two apps of one spec got the same
// UI and firmware port) and to outgoing connections of other workers, so the app then
// died with "bind: address already in use" during startup.
const BLOCK = 500;
const firstPort = 20_000 + (Number(process.env.TEST_PARALLEL_INDEX ?? 0) % 24) * BLOCK;
let nextOffset = process.pid % BLOCK;

/** freePort returns an unused loopback port that no earlier call of this worker handed out. */
export async function freePort(): Promise<number> {
  for (let tries = 0; tries < BLOCK; tries++) {
    const port = firstPort + (nextOffset++ % BLOCK);
    if (await canBind(port)) return port;
  }
  throw new Error(`no free port in ${firstPort}..${firstPort + BLOCK - 1}`);
}

function canBind(port: number): Promise<boolean> {
  return new Promise((ok) => {
    const s = netServer();
    s.once("error", () => ok(false));
    s.listen(port, "127.0.0.1", () => s.close(() => ok(true)));
  });
}

export interface Devenv {
  vars: Record<string, string>;
  devices: Record<string, { addr: string; topic: string; profile: string }>;
  stop(): Promise<void>;
}

/** startDevenv runs tasmofleet-devenv and parses its KEY=value output until DEVENV_READY. */
export async function startDevenv(args: string[] = []): Promise<Devenv> {
  const proc = spawn(devenvBinary, ["-fixtures", join(root, "testdata/fixtures"), "-time-scale", "0.05", ...args], {
    stdio: ["ignore", "pipe", "inherit"],
  });
  const vars: Record<string, string> = {};
  const devices: Devenv["devices"] = {};
  await new Promise<void>((ok, fail) => {
    const rl = createInterface({ input: proc.stdout! });
    const timer = setTimeout(() => fail(new Error("devenv did not become ready")), 30_000);
    proc.once("exit", (c) => fail(new Error(`devenv exited early (${c})`)));
    rl.on("line", (line) => {
      if (line.startsWith("DEVENV_DEVICE ")) {
        const f = Object.fromEntries(line.slice(14).split(" ").map((kv) => kv.split("=") as [string, string]));
        devices[decodeURIComponent(f.name)] = { addr: f.addr, topic: f.topic, profile: f.profile };
      } else if (line === "DEVENV_READY=1") {
        clearTimeout(timer);
        ok();
      } else {
        const i = line.indexOf("=");
        if (i > 0 && !line.startsWith("#")) vars[line.slice(0, i)] = line.slice(i + 1);
      }
    });
  });
  return { vars, devices, stop: () => stopProcess(proc) };
}

export interface App {
  url: string;
  dataDir: string;
  env: Record<string, string>;
  /** run executes a CLI subcommand against the same data directory. */
  cli(args: string[], input?: string): string;
  /** setupLink is the first-start setup link the app printed to its log (N12), "" when none. */
  setupLink(): string;
  /** stop ends the process; keepData leaves the data dir for a restart. */
  stop(keepData?: boolean): Promise<void>;
}

export interface AppOptions {
  port?: number;
  env?: Record<string, string>;
  /** keep the data dir of an earlier app (restart scenarios) */
  dataDir?: string;
  /** bare: only the data directory, the listener port and the log format; everything else is the product default (a fresh install) */
  bare?: boolean;
  /** scheme the app is served with (https for TLS modes), default http */
  scheme?: "http" | "https";
}

/** startApp runs the real tasmofleet binary with a fresh data directory. */
export async function startApp(opts: AppOptions = {}): Promise<App> {
  const port = opts.port ?? (await freePort());
  const dataDir = opts.dataDir ?? mkdtempSync(join(tmpdir(), "tasmofleet-e2e-"));
  const env: Record<string, string> = {
    PATH: process.env.PATH ?? "",
    TASMOFLEET_DATA_DIR: dataDir,
    TASMOFLEET_CONFIG_FILE: "/dev/null",
    TASMOFLEET_LISTEN: `127.0.0.1:${port}`,
    ...(opts.bare ? {} : { TASMOFLEET_DEVICE_NETWORKS: "127.0.0.0/8", TASMOFLEET_DECODER_PATH: "" }),
    TASMOFLEET_LOG_FORMAT: "text",
    ...opts.env,
  };
  const proc = spawn(binary, ["serve"], { env, stdio: ["ignore", "pipe", "pipe"] });
  // Keep the tail of the output so a startup failure explains itself.
  const tail = captureTail(proc);
  let link = "";
  for (const stream of [proc.stdout, proc.stderr])
    if (stream)
      createInterface({ input: stream }).on("line", (line) => {
        // text log: setup="https://host:port/setup/account?token=..." (quoted: the value has "=")
        const m = /\bsetup="?([^"\s]+)/.exec(line);
        if (m) link = m[1];
      });
  const scheme = opts.scheme ?? "http";
  const base = `${scheme}://127.0.0.1:${port}`;
  const basePath = env.TASMOFLEET_BASE_PATH ?? "";
  await waitHealthy(`${base}${basePath}/healthz`, proc, tail);
  return {
    url: base + basePath,
    dataDir,
    env,
    cli: (args, input) => execFileSync(binary, args, { env, input, encoding: "utf8" }),
    setupLink: () => link,
    stop: async (keepData = false) => {
      await stopProcess(proc);
      if (!opts.dataDir && !keepData) rmSync(dataDir, { recursive: true, force: true });
    },
  };
}

/** captureTail records the last lines of stdout and stderr (echoed live when E2E_LOGS is set). */
function captureTail(proc: ChildProcess, max = 50): () => string {
  const lines: string[] = [];
  for (const [name, stream, out] of [["stdout", proc.stdout, process.stdout], ["stderr", proc.stderr, process.stderr]] as const) {
    if (!stream) continue;
    createInterface({ input: stream }).on("line", (line) => {
      lines.push(`${name}: ${line}`);
      if (lines.length > max) lines.shift();
      if (process.env.E2E_LOGS) out.write(line + "\n");
    });
  }
  return () => lines.join("\n");
}

async function waitHealthy(url: string, proc: ChildProcess, tail: () => string): Promise<void> {
  const deadline = Date.now() + 30_000;
  let exited: string | undefined;
  proc.once("exit", (code, signal) => (exited = `exit code ${code}, signal ${signal}`));
  while (Date.now() < deadline) {
    if (exited) {
      // Let the readline interfaces flush the final lines before reporting.
      await new Promise((r) => setTimeout(r, 200));
      throw new Error(`tasmofleet exited during startup (${exited}), last output:\n${tail()}`);
    }
    try {
      if ((await status(url)) === 200) return;
    } catch {
      /* not up yet */
    }
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(`${url} never became healthy, last output:\n${tail()}`);
}

/** status GETs url (self-signed certificates accepted) and returns the HTTP status. */
function status(url: string): Promise<number> {
  return new Promise((ok, fail) => {
    const get = url.startsWith("https:") ? httpsRequest : httpRequest;
    const req = get(url, { rejectUnauthorized: false }, (res) => {
      res.resume();
      ok(res.statusCode ?? 0);
    });
    req.on("error", fail);
    req.end();
  });
}

function stopProcess(proc: ChildProcess): Promise<void> {
  return new Promise((ok) => {
    if (proc.exitCode !== null || proc.signalCode !== null) return ok();
    proc.once("exit", () => ok());
    proc.kill("SIGTERM");
    setTimeout(() => proc.kill("SIGKILL"), 5_000).unref();
  });
}

/**
 * startProxy is a tiny forward-auth stand-in for Traefik + Authentik: it
 * forwards to the app from 127.0.0.2 (the only trusted proxy address) and
 * adds X-authentik-* headers from the X-Test-User / X-Test-Groups request
 * headers of the test client.
 */
export function startProxy(target: string, prefix = ""): Promise<{ url: string; server: Server }> {
  const t = new URL(target);
  const server = createServer((req: IncomingMessage, res: ServerResponse) => {
    const headers = { ...req.headers };
    delete headers["x-authentik-username"];
    delete headers["x-authentik-groups"];
    if (typeof req.headers["x-test-user"] === "string") headers["x-authentik-username"] = req.headers["x-test-user"];
    if (typeof req.headers["x-test-groups"] === "string") headers["x-authentik-groups"] = req.headers["x-test-groups"];
    if (prefix) headers["x-forwarded-prefix"] = prefix;
    const up = httpRequest(
      { host: t.hostname, port: t.port, path: req.url, method: req.method, headers, localAddress: "127.0.0.2" },
      (r) => {
        res.writeHead(r.statusCode ?? 502, r.headers);
        r.pipe(res);
      },
    );
    up.on("error", () => {
      res.statusCode = 502;
      res.end();
    });
    req.pipe(up);
  });
  return new Promise((ok) => server.listen(0, "127.0.0.1", () => ok({ url: `http://127.0.0.1:${(server.address() as AddressInfo).port}`, server })));
}

/** writeTasmoAdminDir writes a devices.csv for the devenv fleet (TasmoAdmin layout) and returns its directory. */
export function writeTasmoAdminDir(devices: Devenv["devices"]): string {
  const dir = mkdtempSync(join(tmpdir(), "tasmofleet-ta-"));
  const rows = Object.entries(devices).map(([name, d], i) => {
    const [host, port] = d.addr.split(":");
    return `${i + 1},"${name}",${host},,,bulb_1,1,1,0,0,1,${port},"${name}",0,${d.topic},0`;
  });
  writeFileSync(join(dir, "devices.csv"), rows.join("\n") + "\n");
  return dir;
}
