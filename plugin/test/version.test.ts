import { expect, test } from "bun:test";
import pkg from "../package.json";
import { VERSION } from "../src/version.ts";

test("plugin version comes from VERSION and package metadata stays in sync", () => {
	expect(VERSION).toMatch(/^\d+\.\d+\.\d+$/);
	expect(pkg.version).toBe(VERSION);
	expect(pkg.omp.version).toBe(VERSION);
});
