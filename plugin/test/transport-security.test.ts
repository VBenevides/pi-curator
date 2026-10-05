import { expect, test } from "bun:test";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { memoryCommand } from "../src/curator.ts";

test("transport rejects byte overflow without reflecting payload", async () => {
 const dir = mkdtempSync(join(tmpdir(), "curator-transport-"));
 try {
  const bin = join(dir, "child");
  writeFileSync(bin, '#!/usr/bin/env node\nprocess.stdout.write("SECRET".repeat(200000));\n', { mode: 0o700 });
  await expect(memoryCommand({ cwd: dir, bin }, ["read"])).rejects.toThrow("output limit");
 } finally { rmSync(dir, { recursive: true, force: true }); }
});

test("transport decodes split UTF-8 only after collecting bounded bytes", async () => {
 const dir = mkdtempSync(join(tmpdir(), "curator-transport-"));
 try {
  const bin = join(dir, "child");
  writeFileSync(bin, '#!/usr/bin/env node\nprocess.stdout.write(Buffer.from([0xe9]), () => process.stdout.write(Buffer.from([0x9b,0xaa])));\n', { mode: 0o700 });
  expect(await memoryCommand({ cwd: dir, bin }, ["read"])).toBe("雪");
 } finally { rmSync(dir, { recursive: true, force: true }); }
});
