import { describe, expect, test } from "bun:test";
import { isMemoryRecall } from "../src/recall.ts";

describe("isMemoryRecall", () => {
	test("detects rg/jq/cat over the memory directory and read-only curator commands", () => {
		for (const command of [
			'rg -i "deploy" .curator/memory/active.jsonl',
			"jq -c 'select(.kind==\"episode\")' .curator/memory/active.jsonl",
			"cat ./.curator/journal/events.jsonl | head",
			"cd /repo && curator show mem_1234",
			"curator list --state archived",
			"curator search --file config.local.ini retry",
		]) {
			expect(isMemoryRecall({ command })).toBe(true);
		}
	});

	test("detects structured read and search tools pointed at memory", () => {
		expect(isMemoryRecall({ path: ".curator/memory/active.jsonl" })).toBe(true);
		expect(isMemoryRecall({ paths: ["src", "/repo/.curator/"] })).toBe(true);
	});

	test("ignores unrelated calls and look-alikes", () => {
		for (const input of [
			{ command: "rg foo src/" },
			{ command: "ls .curatorial" },
			{ command: "echo mycurator list" },
			{ path: "docs/curator.md" },
			{ command: "curator --version" },
			"not a record",
			null,
		]) {
			expect(isMemoryRecall(input)).toBe(false);
		}
	});
});
