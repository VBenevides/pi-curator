import { describe, expect, test } from "bun:test";
import { CallPaths, eventsFromMessage, safeId, taskBoundary } from "../src/map.ts";

describe("eventsFromMessage", () => {
	test("captures visible text and never thinking blocks", () => {
		const calls = new CallPaths();
		const events = eventsFromMessage(
			"s1",
			{ role: "assistant", timestamp: 5, content: [{ type: "thinking", thinking: "private chain" }, { type: "text", text: "fixed it" }] },
			calls,
		);
		expect(events).toHaveLength(1);
		expect(events[0]).toMatchObject({ category: "assistant_message", content: "fixed it", id: "assistant:5" });
		expect(JSON.stringify(events)).not.toContain("private chain");
	});

	test("tool calls and results are linked by call id and carry paths for denial checks", () => {
		const calls = new CallPaths();
		const [, call] = eventsFromMessage(
			"s1",
			{ role: "assistant", timestamp: 6, content: [{ type: "text", text: "searching" }, { type: "toolCall", id: "toolu_01AbC", name: "grep", arguments: { path: "cfg/.env" } }] },
			calls,
		);
		expect(call).toMatchObject({ category: "tool_call", call_id: "toolu_01AbC", paths: ["cfg/.env"], id: "call:toolu_01AbC" });
		const [result] = eventsFromMessage("s1", { role: "toolResult", toolCallId: "toolu_01AbC", toolName: "grep", content: [{ type: "text", text: "KEY=1" }] }, calls);
		expect(result).toMatchObject({ category: "tool_result", paths: ["cfg/.env"], content: "KEY=1", id: "result:toolu_01AbC" });
	});

	test("read results are not captured but the read call still records the path", () => {
		const calls = new CallPaths();
		const events = eventsFromMessage(
			"s1",
			{ role: "assistant", timestamp: 7, content: [{ type: "toolCall", id: "toolu_02", name: "read", arguments: { path: "src/a.ts" } }] },
			calls,
		);
		expect(events).toMatchObject([{ category: "tool_call", tool_name: "read", paths: ["src/a.ts"] }]);
		expect(eventsFromMessage("s1", { role: "toolResult", toolCallId: "toolu_02", toolName: "read", content: [{ type: "text", text: "file body" }] }, calls)).toEqual([]);
		expect(calls.take("toolu_02")).toEqual([]);
	});

	test("non-transcript roles and empty messages produce nothing", () => {
		const calls = new CallPaths();
		for (const message of [{ role: "custom", content: "x" }, { role: "user", content: [] }, { role: "bashExecution" }, null, "text"]) {
			expect(eventsFromMessage("s", message, calls)).toEqual([]);
		}
	});

	test("ids are stable for replays and always valid curator ids", () => {
		const message = { role: "user", timestamp: 9, content: "hello" };
		const a = eventsFromMessage("s", message, new CallPaths());
		const b = eventsFromMessage("s", message, new CallPaths());
		expect(a).toEqual(b);
		expect(safeId("toolu x:y")).toBe("toolu-x:y");
		expect(safeId("_leading")).toMatch(/^h-[0-9a-f]{16}$/);
		expect(safeId("x".repeat(300))).toMatch(/^h-[0-9a-f]{16}$/);
	});

	test("task boundary id follows the last message", () => {
		expect(taskBoundary("s", [{ timestamp: 11 }]).id).toBe("task:11");
	});
});
