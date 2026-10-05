import type { EventIn } from "./map.ts";

export interface GapIn {
	session_id: string;
	event_id: string;
}

export interface IngestRequest {
	events: EventIn[];
	gaps: GapIn[];
}

export interface ItemResult {
	id: string;
	outcome: "durable" | "duplicate" | "rejected" | "failed";
	code?: string;
	error?: string;
}

export interface IngestResponse {
	results: ItemResult[];
	gaps?: ItemResult[];
}

/** Sends one request to curator; throws when no valid response was obtained. */
export type Transport = (request: IngestRequest) => Promise<IngestResponse>;

export interface CaptureOptions {
	/** Maximum events held in memory before overflow becomes explicit gaps. */
	maxQueue?: number;
	/** Events per curator call; must not exceed the curator batch limit (200). */
	batchSize?: number;
	/** Consecutive failures before an event is declared a gap. */
	maxAttempts?: number;
	/** Cap on remembered gaps; beyond it only a counter grows. */
	maxGaps?: number;
	onProblem?: (message: string) => void;
}

interface Pending {
	event: EventIn;
	attempts: number;
}

export interface CaptureStats {
	queued: number;
	gaps: number;
	gapsUncounted: number;
	rejected: number;
	durable: number;
}

/**
 * Bounded, single-flight capture queue. Acknowledgement means curator reported
 * durable or duplicate; anything else stays visible as a retry or a gap, and
 * nothing is claimed durable from the queue alone.
 */
export class Capture {
	readonly #send: Transport;
	readonly #maxQueue: number;
	readonly #batchSize: number;
	readonly #maxAttempts: number;
	readonly #maxGaps: number;
	readonly #problem: (message: string) => void;
	#queue: Pending[] = [];
	#gaps: GapIn[] = [];
	#flight: Promise<void> | undefined;
	#rejected = 0;
	#durable = 0;
	#gapsUncounted = 0;

	constructor(send: Transport, options: CaptureOptions = {}) {
		this.#send = send;
		this.#maxQueue = options.maxQueue ?? 1000;
		this.#batchSize = Math.min(options.batchSize ?? 100, 200);
		this.#maxAttempts = options.maxAttempts ?? 3;
		this.#maxGaps = options.maxGaps ?? 1000;
		this.#problem = options.onProblem ?? (() => {});
	}

	enqueue(events: EventIn[]): void {
		for (const event of events) {
			if (this.#queue.length >= this.#maxQueue) {
				this.#recordGap(event, "queue full");
				continue;
			}
			this.#queue.push({ event, attempts: 0 });
		}
	}

	/** Turn still-queued events into gaps; they are never silently dropped. */
	abandonQueue(): void {
		const left = this.#queue;
		this.#queue = [];
		for (const p of left) this.#recordGap(p.event, "not delivered before the session ended");
	}

	stats(): CaptureStats {
		return { queued: this.#queue.length, gaps: this.#gaps.length, gapsUncounted: this.#gapsUncounted, rejected: this.#rejected, durable: this.#durable };
	}

	/** Gaps not yet acknowledged by curator. */
	pendingGaps(): readonly GapIn[] {
		return this.#gaps;
	}

	#recordGap(event: EventIn, why: string): void {
		this.#problem(`capture gap ${event.session_id}/${event.id}: ${why}`);
		if (this.#gaps.length >= this.#maxGaps) {
			this.#gapsUncounted++;
			return;
		}
		this.#gaps.push({ session_id: event.session_id, event_id: event.id });
	}

	/**
	 * Drain the queue and pending gaps. Single-flight: concurrent callers share
	 * one drain. Stops at the deadline or after a transport failure so shutdown
	 * stays bounded; whatever remains stays queued and visible in stats().
	 */
	async flush(deadlineMs?: number): Promise<void> {
		this.#flight ??= this.#drain(deadlineMs === undefined ? Number.POSITIVE_INFINITY : Date.now() + deadlineMs).finally(() => {
			this.#flight = undefined;
		});
		if (deadlineMs === undefined) return this.#flight;
		// A drain already in flight may have no deadline; the caller's bound still holds.
		const { promise, resolve } = Promise.withResolvers<void>();
		const timer = setTimeout(resolve, deadlineMs);
		try {
			await Promise.race([this.#flight, promise]);
		} finally {
			clearTimeout(timer);
		}
	}

	async #drain(deadline: number): Promise<void> {
		while ((this.#queue.length > 0 || this.#gaps.length > 0) && Date.now() < deadline) {
			const batch = this.#queue.slice(0, this.#batchSize);
			const gaps = this.#gaps.slice(0, this.#batchSize);
			let response: IngestResponse;
			try {
				response = await this.#send({ events: batch.map(p => p.event), gaps });
			} catch (error) {
				this.#problem(`curator unavailable: ${error instanceof Error ? error.message : String(error)}`);
				for (const p of batch) p.attempts++;
				this.#expire();
				return; // retry on the next flush; never spin on a dead transport
			}
			if (!this.#apply(batch, gaps, response)) return; // no progress: wait for the next flush instead of looping
		}
	}

	#expire(): void {
		const keep: Pending[] = [];
		for (const p of this.#queue) {
			if (p.attempts >= this.#maxAttempts) this.#recordGap(p.event, "curator unreachable");
			else keep.push(p);
		}
		this.#queue = keep;
	}

	#apply(batch: Pending[], gaps: GapIn[], response: IngestResponse): boolean {
		const done = new Set<Pending>();
		batch.forEach((p, i) => {
			const result = response.results[i];
			if (result === undefined || result.id !== p.event.id) {
				p.attempts++;
				this.#problem(`curator response does not match event ${p.event.id}`);
				return;
			}
			if (result.outcome === "durable" || result.outcome === "duplicate") {
				this.#durable++;
				done.add(p);
			} else if (result.outcome === "rejected") {
				this.#rejected++;
				this.#problem(`event ${p.event.id} rejected: ${result.code ?? ""} ${result.error ?? ""}`.trim());
				done.add(p); // permanent: retrying cannot change the verdict
			} else {
				p.attempts++;
				this.#problem(`event ${p.event.id} failed: ${result.code ?? ""} ${result.error ?? ""}`.trim());
			}
		});
		this.#queue = this.#queue.filter(p => !done.has(p));
		this.#expire();
		const gapsDone = new Set<number>();
		gaps.forEach((_, i) => {
			const outcome = response.gaps?.[i]?.outcome;
			if (outcome === "durable" || outcome === "duplicate" || outcome === "rejected") gapsDone.add(i);
		});
		this.#gaps = this.#gaps.filter(g => !gaps.includes(g) || !gapsDone.has(gaps.indexOf(g)));
		return done.size > 0 || gapsDone.size > 0;
	}
}
