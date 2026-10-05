import { spawn } from "node:child_process";
import type { IngestRequest, IngestResponse, Transport } from "./capture.ts";

export interface CuratorOptions {
	/** Curator executable; defaults to PI_CURATOR_BIN or `curator` on PATH. */
	bin?: string;
	cwd: string;
	timeoutMs?: number;
}

export type RepoState = "pending_consent" | "needs_ignore" | "tracked" | "ready" | "not_git";

export interface RepoStatus {
	state: RepoState;
	root?: string;
	store?: string;
	repo_id?: string;
	message?: string;
}

interface Run {
	code: number | null;
	stdout: string;
	stderr: string;
}

const MAX_OUTPUT = 1 << 20;

function binOf(options: CuratorOptions): string {
	return options.bin ?? process.env.PI_CURATOR_BIN ?? "curator";
}

/** Run curator with a hard timeout and bounded output; never throws on non-zero exit. */
function run(options: CuratorOptions, args: string[], stdin?: string): Promise<Run> {
	const command = args[0];
	if (command === undefined) throw new Error("curator command is required");
	const timeoutMs = options.timeoutMs ?? 15_000;
	const { promise, resolve, reject } = Promise.withResolvers<Run>();
	const child = spawn(binOf(options), [command, "--cwd", options.cwd, ...args.slice(1)], { stdio: ["pipe", "pipe", "pipe"] });
	const stdout: Buffer[] = [];
	const stderr: Buffer[] = [];
	let outputBytes = 0;
	let failed = false;
	const collect = (chunks: Buffer[], data: Buffer): void => {
		if (failed) return;
		if (data.length > MAX_OUTPUT - outputBytes) {
			failed = true;
			clearTimeout(timer);
			child.kill("SIGKILL");
			reject(new Error("curator output limit exceeded"));
			return;
		}
		outputBytes += data.length;
		chunks.push(data);
	};
	const timer = setTimeout(() => {
		child.kill("SIGKILL");
		reject(new Error(`curator ${args[0]} timed out after ${timeoutMs}ms`));
	}, timeoutMs);
	child.stdout.on("data", (d: Buffer) => collect(stdout, d));
	child.stderr.on("data", (d: Buffer) => collect(stderr, d));
	child.on("error", error => {
		clearTimeout(timer);
		reject(new Error(`cannot run curator (${binOf(options)}): ${error.message}`));
	});
	child.on("close", code => {
		clearTimeout(timer);
		resolve({ code, stdout: Buffer.concat(stdout).toString("utf8"), stderr: Buffer.concat(stderr).toString("utf8") });
	});
	child.stdin.on("error", () => {}); // a dead child is reported via close/error
	child.stdin.end(stdin ?? "");
	return promise;
}

/** Transport that pipes each request to `curator ingest`. */
export function ingestTransport(options: CuratorOptions): Transport {
	return async (request: IngestRequest): Promise<IngestResponse> => {
		const res = await run(options, ["ingest"], JSON.stringify(request));
		let parsed: unknown;
		try {
			parsed = JSON.parse(res.stdout);
		} catch {
			throw new Error(`curator ingest produced no JSON response (exit ${res.code})`);
		}
		if (typeof parsed !== "object" || parsed === null || !("results" in parsed) || !Array.isArray(parsed.results)) {
			throw new Error(`curator ingest returned an unexpected response (exit ${res.code})`);
		}
		return parsed as IngestResponse;
	};
}

function responseJSON(text: string): unknown {
	try {
		return JSON.parse(text);
	} catch {
		throw new Error("curator produced an invalid JSON response");
	}
}

export async function repoStatus(options: CuratorOptions): Promise<RepoStatus> {
	const res = await run(options, ["status"]);
	if (res.code !== 0) throw new Error(`curator status failed (exit ${res.code})`);
	return responseJSON(res.stdout) as RepoStatus;
}

/** Initialize memory; callers must have obtained explicit consent first. */
export async function initRepo(options: CuratorOptions): Promise<RepoStatus> {
	const res = await run(options, ["init", "--consent"]);
	if (res.code !== 0) throw new Error(`curator init failed (exit ${res.code})`);
	return responseJSON(res.stdout) as RepoStatus;
}

interface DecisionHit {
	created_at_utc: string;
	snippet: string;
}

/**
 * The newest user decisions in the journal, oldest first, as plain text lines.
 * Only used when the operator opts in; the text is stored history, so callers
 * must present it as untrusted data.
 */
export async function recentDecisions(options: CuratorOptions, count: number): Promise<string[]> {
	const res = await run(options, ["search", "--decisions", "--limit", String(count), "--snippet", "300"]);
	if (res.code !== 0) throw new Error(`curator search failed (exit ${res.code})`);
	const parsed = responseJSON(res.stdout) as { sessions?: { hits: DecisionHit[] }[] };
	// Curator returns newest first; reversing before the stable sort keeps that order when timestamps tie.
	const hits = (parsed.sessions ?? []).flatMap(s => s.hits).reverse().sort((a, b) => a.created_at_utc.localeCompare(b.created_at_utc));
	return hits.map(h => `${h.created_at_utc.slice(0, 10)}: ${h.snippet}`);
}

/** Read-only memory command; nonzero exits remain observable to the agent. */
export async function memoryCommand(options: CuratorOptions, args: string[]): Promise<string> {
	const res = await run(options, args);
	if (res.code !== 0) throw new Error(`curator ${args[0]} failed (exit ${res.code})`);
	return res.stdout.trim();
}
