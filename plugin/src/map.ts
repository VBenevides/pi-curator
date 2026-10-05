import { createHash } from "node:crypto";

/** Wire shape accepted by `curator ingest`. */
export interface EventIn {
	id: string;
	session_id: string;
	category: "user_message" | "assistant_message" | "tool_call" | "tool_result" | "task_boundary";
	role?: string;
	tool_name?: string;
	call_id?: string;
	content?: string;
	is_error?: boolean;
	paths?: string[];
	source?: { host: string; entry_id?: string };
}

const MAX_ID = 128;
const PATH_KEYS = new Set(["path", "file", "file_path", "filepath", "paths", "files"]);

/** Curator IDs allow `[A-Za-z0-9._:@/-]`; host IDs such as `toolu_x` do not. */
export function safeId(raw: string): string {
	const cleaned = raw.replace(/[^A-Za-z0-9._:@/-]/g, "-");
	if (/^[A-Za-z0-9]/.test(cleaned) && cleaned.length <= MAX_ID) return cleaned;
	const digest = createHash("sha256").update(raw).digest("hex").slice(0, 16);
	return `h-${digest}`;
}

export function isRecord(value: unknown): value is Record<string, unknown> {
	return typeof value === "object" && value !== null;
}

/** Visible text only: thinking blocks and non-text blocks are never captured. */
function visibleText(content: unknown): string {
	if (typeof content === "string") return content;
	if (!Array.isArray(content)) return "";
	const parts: string[] = [];
	for (const block of content) {
		if (isRecord(block) && block.type === "text" && typeof block.text === "string") parts.push(block.text);
	}
	return parts.join("\n");
}

function pathsOf(args: unknown): string[] {
	if (!isRecord(args)) return [];
	const out: string[] = [];
	for (const [key, value] of Object.entries(args)) {
		if (!PATH_KEYS.has(key)) continue;
		if (typeof value === "string") out.push(value);
		else if (Array.isArray(value)) for (const v of value) if (typeof v === "string") out.push(v);
	}
	return out;
}

function stamp(message: Record<string, unknown>, role: string, text: string): string {
	if (typeof message.timestamp === "number") return `${role}:${message.timestamp}`;
	return `${role}:${createHash("sha256").update(text).digest("hex").slice(0, 16)}`;
}

/** Remembers the paths a tool call named so its result can be filtered by them. */
export class CallPaths {
	readonly #max: number;
	readonly #map = new Map<string, string[]>();
	constructor(max = 256) {
		this.#max = max;
	}
	set(callId: string, paths: string[]): void {
		if (paths.length === 0) return;
		this.#map.set(callId, paths);
		if (this.#map.size > this.#max) {
			const oldest = this.#map.keys().next().value;
			if (oldest !== undefined) this.#map.delete(oldest);
		}
	}
	take(callId: string): string[] {
		const paths = this.#map.get(callId) ?? [];
		this.#map.delete(callId);
		return paths;
	}
}

/**
 * Convert one finished host message into curator events. Roles that are not
 * part of the visible transcript are ignored; private reasoning is never read.
 */
export function eventsFromMessage(sessionId: string, message: unknown, calls: CallPaths): EventIn[] {
	if (!isRecord(message) || typeof message.role !== "string") return [];
	const source = { host: "omp" };
	switch (message.role) {
		case "user": {
			const content = visibleText(message.content);
			if (content === "") return [];
			return [{ id: safeId(stamp(message, "user", content)), session_id: sessionId, category: "user_message", role: "user", content, source }];
		}
		case "assistant": {
			const events: EventIn[] = [];
			const content = visibleText(message.content);
			if (content !== "") {
				events.push({ id: safeId(stamp(message, "assistant", content)), session_id: sessionId, category: "assistant_message", role: "assistant", content, source });
			}
			if (Array.isArray(message.content)) {
				for (const block of message.content) {
					if (!isRecord(block) || block.type !== "toolCall" || typeof block.id !== "string" || typeof block.name !== "string") continue;
					const paths = pathsOf(block.arguments);
					calls.set(block.id, paths);
					events.push({
						id: safeId(`call:${block.id}`),
						session_id: sessionId,
						category: "tool_call",
						tool_name: block.name,
						call_id: block.id,
						content: JSON.stringify(block.arguments ?? {}),
						...(paths.length > 0 ? { paths } : {}),
						source,
					});
				}
			}
			return events;
		}
		case "toolResult": {
			if (typeof message.toolCallId !== "string") return [];
			const paths = calls.take(message.toolCallId);
			// File contents stay on disk, and the matching tool_call already records the path.
			if (message.toolName === "read") return [];
			return [
				{
					id: safeId(`result:${message.toolCallId}`),
					session_id: sessionId,
					category: "tool_result",
					tool_name: typeof message.toolName === "string" ? message.toolName : undefined,
					call_id: message.toolCallId,
					content: visibleText(message.content),
					...(typeof message.isError === "boolean" ? { is_error: message.isError } : {}),
					...(paths.length > 0 ? { paths } : {}),
					source,
				},
			];
		}
		default:
			return [];
	}
}

/** A task boundary closes the episode when the agent loop ends. */
export function taskBoundary(sessionId: string, messages: unknown): EventIn {
	let last = "end";
	if (Array.isArray(messages)) {
		const tail = messages[messages.length - 1];
		if (isRecord(tail) && typeof tail.timestamp === "number") last = String(tail.timestamp);
	}
	return { id: safeId(`task:${last}`), session_id: sessionId, category: "task_boundary", source: { host: "omp" } };
}
