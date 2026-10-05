import { describe, expect, test } from "bun:test";
import { Capture, type IngestRequest, type IngestResponse, type Transport } from "../src/capture.ts";
import type { EventIn } from "../src/map.ts";

function ev(id: string): EventIn {
	return { id, session_id: "s", category: "user_message", content: id };
}

/** Fake curator: acknowledges every item with the outcome chosen per event id. */
function curator(outcome: (id: string) => "durable" | "duplicate" | "rejected" | "failed", seen: IngestRequest[] = []): Transport {
	return async (req: IngestRequest): Promise<IngestResponse> => {
		seen.push(req);
		return {
			results: req.events.map(e => ({ id: e.id, outcome: outcome(e.id) })),
			gaps: req.gaps.map(g => ({ id: `gap_${g.event_id}`, outcome: "durable" as const })),
		};
	};
}

describe("Capture", () => {
	test("acknowledges only what curator reports durable, in bounded batches", async () => {
		const seen: IngestRequest[] = [];
		const capture = new Capture(curator(() => "durable", seen), { batchSize: 2 });
		capture.enqueue([ev("a"), ev("b"), ev("c")]);
		await capture.flush();
		expect(seen.map(r => r.events.length)).toEqual([2, 1]);
		expect(capture.stats()).toMatchObject({ queued: 0, durable: 3, gaps: 0 });
	});

	test("queue overflow becomes explicit gaps, never silent loss, and is reported to curator", async () => {
		const seen: IngestRequest[] = [];
		const capture = new Capture(curator(() => "durable", seen), { maxQueue: 2 });
		capture.enqueue([ev("a"), ev("b"), ev("c"), ev("d")]);
		expect(capture.stats()).toMatchObject({ queued: 2, gaps: 2 });
		await capture.flush();
		expect(seen.flatMap(r => r.gaps.map(g => g.event_id))).toEqual(["c", "d"]);
		expect(capture.stats()).toMatchObject({ queued: 0, gaps: 0, durable: 2 });
	});

	test("an unreachable curator keeps events, stops after one try per flush, then declares gaps", async () => {
		let calls = 0;
		const capture = new Capture(async () => {
			calls++;
			throw new Error("down");
		}, { maxAttempts: 2 });
		capture.enqueue([ev("a")]);
		await capture.flush();
		expect(calls).toBe(1);
		expect(capture.stats()).toMatchObject({ queued: 1, gaps: 0 });
		await capture.flush();
		expect(capture.stats()).toMatchObject({ queued: 0, gaps: 1 });
		expect(capture.pendingGaps()).toEqual([{ session_id: "s", event_id: "a" }]);
	});

	test("one failed or rejected event does not discard its successful neighbours", async () => {
		const capture = new Capture(curator(id => (id === "bad" ? "rejected" : id === "slow" ? "failed" : "durable")), { maxAttempts: 5 });
		capture.enqueue([ev("a"), ev("bad"), ev("slow"), ev("b")]);
		await capture.flush();
		expect(capture.stats()).toMatchObject({ durable: 2, rejected: 1, queued: 1 });
	});

	test("flush with a deadline returns on time even when curator hangs", async () => {
		const capture = new Capture(() => new Promise(() => {}));
		capture.enqueue([ev("a")]);
		const started = Date.now();
		await capture.flush(50);
		expect(Date.now() - started).toBeLessThan(1000);
		expect(capture.stats().queued).toBe(1);
	});

	test("abandoned queue is journaled as gaps through the transport", async () => {
		const seen: IngestRequest[] = [];
		let up = false;
		const inner = curator(() => "durable", seen);
		const capture = new Capture(req => (up ? inner(req) : Promise.reject(new Error("down"))));
		capture.enqueue([ev("a"), ev("b")]);
		await capture.flush();
		capture.abandonQueue();
		up = true;
		await capture.flush();
		expect(seen.at(-1)?.gaps.map(g => g.event_id)).toEqual(["a", "b"]);
		expect(capture.stats()).toMatchObject({ queued: 0, gaps: 0 });
	});

	test("replaying the same event is harmless", async () => {
		const capture = new Capture(curator(() => "duplicate"));
		capture.enqueue([ev("a"), ev("a")]);
		await capture.flush();
		expect(capture.stats()).toMatchObject({ queued: 0, durable: 2 });
	});
});
