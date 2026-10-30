// Decides which registry images the ghcr-cleanup workflow may delete: for every minor version
// X.Y except the highest one, only the newest X.Y.Z stays (older patches have no rollback value:
// migrations are append-only, so a database migrated by a newer release does not run on an old
// one). Every patch of the highest minor, `latest`, `edge`, pre-releases and any other tag stay.
//
// Pure logic in selectSuperseded(); the CLI reads a JSON array of {id, tags, digest} from stdin
// (one entry per package version on ghcr, one per tag on Docker Hub) and prints the entries to
// delete as JSON. It touches no registry.
import { fileURLToPath } from "node:url";

const STABLE = /^(\d+)\.(\d+)\.(\d+)$/;

function parse(tag) {
  const m = STABLE.exec(tag);
  return m ? { major: +m[1], minor: +m[2], patch: +m[3] } : null;
}

// items: [{id, tags: string[], digest?: string}]. Returns the items to delete.
export function selectSuperseded(items) {
  const stable = items.flatMap((it) => it.tags.map(parse).filter(Boolean));
  if (stable.length === 0) return [];
  const cmp = (a, b) => a.major - b.major || a.minor - b.minor;
  const current = stable.reduce((a, b) => (cmp(a, b) >= 0 ? a : b));
  const newest = new Map(); // "major.minor" -> highest patch
  for (const v of stable) {
    const k = `${v.major}.${v.minor}`;
    newest.set(k, Math.max(newest.get(k) ?? -1, v.patch));
  }
  const superseded = (tag) => {
    const v = parse(tag);
    if (!v) return false;
    if (cmp(v, current) === 0) return false;
    return v.patch < newest.get(`${v.major}.${v.minor}`);
  };
  // An image `latest` points to is never deleted, whatever other tags it carries.
  const latestDigests = new Set(
    items.filter((it) => it.tags.includes("latest") && it.digest).map((it) => it.digest),
  );
  return items.filter(
    (it) =>
      it.tags.length > 0 &&
      it.tags.every(superseded) &&
      !(it.digest && latestDigests.has(it.digest)),
  );
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const chunks = [];
  for await (const c of process.stdin) chunks.push(c);
  console.log(JSON.stringify(selectSuperseded(JSON.parse(Buffer.concat(chunks).toString()))));
}
