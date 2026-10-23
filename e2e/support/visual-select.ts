import { join } from "node:path";
import { root } from "./stack";
import type { Entry } from "./visual-entries";

/** words are the SCREENS filter: entries whose owner or id contains one of them (all when empty). */
const words = (process.env.SCREENS ?? "").split(",").map((w) => w.trim()).filter(Boolean);

export const isSelected = (e: Entry): boolean => words.length === 0 || words.some((w) => e.owner === w || e.id.includes(w));
export const selectedEntries = (all: Entry[]): Entry[] => all.filter(isSelected);

/**
 * stackKey names the app instance of an entry. Entries that change data (a backup is taken, a
 * transaction starts, a password is set) get their own instance of their kind, so the read-only
 * entries never see the change.
 */
export const stackKey = (e: Entry): string => {
  const kind = e.stack ?? "fleet";
  return e.mutates ? kind + "+mutating" : kind;
};

/** stacksFile is where the global setup publishes the running stacks to the workers. */
export const stacksFile = join(root, "e2e/test-results/visual/stacks.json");
