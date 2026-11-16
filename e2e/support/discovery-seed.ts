import { connect } from "node:net";
import { join } from "node:path";
import type { App } from "./stack";

// MQTT discovery proposals for the "New devices" list of Devices -> Add (/devices/new): written into the
// app's database as the discovery would, with the retained last will each device's state comes from.

export interface Proposal {
  mac: string;
  name: string;
  topic: string;
  host: string;
  ip: string;
  /** seconds since the proposal was last seen */
  seen: number;
}

/** publishRetained sends one retained QoS 0 message over a bare MQTT 3.1.1 connection. */
export function publishRetained(url: string, topic: string, payload: string): Promise<void> {
  const { hostname, port } = new URL(url);
  const str = (s: string) => Buffer.concat([Buffer.from([s.length >> 8, s.length & 255]), Buffer.from(s)]);
  const packet = (head: number, body: Buffer) => Buffer.concat([Buffer.from([head, body.length]), body]);
  return new Promise((ok, fail) => {
    const sock = connect(Number(port), hostname, () => {
      sock.write(packet(0x10, Buffer.concat([str("MQTT"), Buffer.from([4, 2, 0, 60]), str("e2e-lwt")])));
    });
    sock.once("data", () => {
      sock.write(packet(0x31, Buffer.concat([str(topic), Buffer.from(payload)])));
      sock.end(Buffer.from([0xe0, 0]), () => ok());
    });
    sock.once("error", fail);
  });
}

/** insertProposals writes the proposals into the app's database (first seen an hour ago). */
export async function insertProposals(app: App, devices: Proposal[]): Promise<void> {
  const { DatabaseSync } = await import("node:sqlite");
  const db = new DatabaseSync(join(app.dataDir, "tasmofleet.db"));
  try {
    db.exec("PRAGMA busy_timeout=5000");
    const now = Date.now();
    for (const d of devices) {
      db.prepare(
        `INSERT INTO discovery_proposals (mac, kind, via, reason, name, address, port, topic, full_topic, existing_id, dismissed, first_seen, seen_at, hostname)
         VALUES (?, 'new', 'mqtt', '', ?, ?, 80, ?, 'tasmota/%topic%/%prefix%/', 0, 0, ?, ?, ?)`,
      ).run(d.mac, d.name, d.ip, d.topic, now - 3600_000, now - d.seen * 1000, d.host);
    }
  } finally {
    db.close();
  }
}
