import { test } from "node:test";
import assert from "node:assert/strict";
import { selectSuperseded } from "./prune-patch-tags.mjs";

const item = (id, ...tags) => ({ id, tags, digest: `sha256:${id}` });
const range = (minor, n) => Array.from({ length: n + 1 }, (_, p) => item(`0.${minor}.${p}`, `0.${minor}.${p}`));
const ids = (xs) => xs.map((x) => x.id);

// Why: only the newest patch of an older minor is a useful rollback target; the current minor
// keeps every patch so a fresh release can always be rolled back one patch.
test("older minors keep only their newest patch, the current minor keeps all", () => {
  const items = [
    ...range(11, 6),
    ...range(12, 3),
    item("0.20.0", "0.20.0", "latest"),
    item("edge", "edge"),
    item("shaonly", "sha-1d9135d"),
  ];
  assert.deepEqual(
    ids(selectSuperseded(items)),
    ["0.11.0", "0.11.1", "0.11.2", "0.11.3", "0.11.4", "0.11.5", "0.12.0", "0.12.1", "0.12.2"],
  );
});

test("every patch of the highest minor stays", () => {
  const items = [...range(20, 2), ...range(19, 2)];
  assert.deepEqual(ids(selectSuperseded(items)), ["0.19.0", "0.19.1"]);
});

// Why: a version with several tags is only deletable if each of them is superseded.
test("a version carrying a non-superseded tag is kept", () => {
  const items = [item("a", "0.12.2", "latest"), item("b", "0.12.3"), item("c", "0.12.1", "edge")];
  assert.deepEqual(ids(selectSuperseded(items)), []);
  assert.deepEqual(ids(selectSuperseded([item("c", "0.12.1", "0.12.0"), item("b", "0.12.3"), item("d", "0.13.0")])), ["c"]);
});

// Why: users who run `latest` must stay pullable even if latest lags behind the newest patch.
test("the image latest points to is never deleted, also not under a second entry", () => {
  const items = [
    { id: "latest", tags: ["latest"], digest: "sha256:x" },
    { id: "0.12.2", tags: ["0.12.2"], digest: "sha256:x" },
    { id: "0.12.3", tags: ["0.12.3"], digest: "sha256:y" },
    item("0.13.0", "0.13.0"),
  ];
  assert.deepEqual(ids(selectSuperseded(items)), []);
});

test("pre-releases, untagged and unrelated tags are left alone", () => {
  const items = [item("rc", "0.12.0-rc1"), { id: "u", tags: [], digest: "sha256:u" }, item("n", "1.0"), item("a", "0.12.0"), item("b", "0.12.1"), item("c", "0.13.0")];
  assert.deepEqual(ids(selectSuperseded(items)), ["a"]);
});

test("nothing to do without stable tags", () => {
  assert.deepEqual(selectSuperseded([item("edge", "edge")]), []);
  assert.deepEqual(selectSuperseded([]), []);
});
