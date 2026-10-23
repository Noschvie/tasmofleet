import { expect, test } from "@playwright/test";
import { freePort } from "../support/stack";

// Regression: freePort used to ask the kernel for port 0 and close it again, so concurrent
// callers got the same port and apps died at startup with "address already in use".
test("freePort never hands out a port twice, and stays below the ephemeral range", async () => {
  const ports = await Promise.all(Array.from({ length: 300 }, () => freePort()));
  expect(new Set(ports).size).toBe(ports.length);
  for (const p of ports) expect(p).toBeLessThan(32_768);
});
