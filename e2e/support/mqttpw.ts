import { join } from "node:path";
import type { App } from "./stack";

/**
 * seedPasswordStates puts the stored password of each named device into the state the test needs (#609): the
 * state words come from what is stored, and the simulator's MQTT-only devices cannot transfer a backup, so no
 * backup would ever teach or test one here. Only the modes that hold no secret are used (broker, empty, unset).
 * Times are milliseconds ago; a name that is not in the inventory is an error.
 */
export interface PasswordSeed {
  mode: "" | "broker" | "empty";
  source?: "" | "backup";
  setAgo?: number;
  okAgo?: number;
  badAgo?: number;
}

export async function seedPasswordStates(app: App, seeds: Record<string, PasswordSeed>): Promise<void> {
  const { DatabaseSync } = await import("node:sqlite");
  const db = new DatabaseSync(join(app.dataDir, "tasmofleet.db"));
  try {
    db.exec("PRAGMA busy_timeout=5000");
    const now = Date.now();
    const ago = (ms?: number) => (ms === undefined ? null : now - ms);
    const update = db.prepare(
      `UPDATE devices SET mqtt_password='', mqtt_password_mode=?, mqtt_password_source=?, mqtt_password_set_at=?,
         mqtt_password_ok_at=?, mqtt_password_bad_at=? WHERE name=?`,
    );
    for (const [name, s] of Object.entries(seeds)) {
      const r = update.run(s.mode, s.source ?? "", ago(s.setAgo), ago(s.okAgo), ago(s.badAgo), name);
      if (r.changes !== 1) throw new Error(`seedPasswordStates: no device named ${name}`);
    }
  } finally {
    db.close();
  }
}

/** oursCount is how many of the named devices are "ours" on the broker (their first MQTT messages arrived). */
async function oursCount(app: App, names: string[]): Promise<number> {
  const { DatabaseSync } = await import("node:sqlite");
  const db = new DatabaseSync(join(app.dataDir, "tasmofleet.db"), { readOnly: true });
  try {
    db.exec("PRAGMA busy_timeout=5000");
    const rows = db.prepare("SELECT name, mqtt_capability FROM devices").all() as { name: string; mqtt_capability: string }[];
    return rows.filter((r) => names.includes(r.name) && r.mqtt_capability === "ours").length;
  } finally {
    db.close();
  }
}

/** waitForOurs polls once a second, at most timeoutSec times, until every named device is "ours" on the broker. */
export async function waitForOurs(app: App, names: string[], timeoutSec = 120): Promise<void> {
  let ours = 0;
  for (let i = 0; i < timeoutSec; i++) {
    ours = await oursCount(app, names);
    if (ours === names.length) return;
    await new Promise((ok) => setTimeout(ok, 1_000));
  }
  throw new Error(`only ${ours} of ${names.length} devices became ours on the broker`);
}
