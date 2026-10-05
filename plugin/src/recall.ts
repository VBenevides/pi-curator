import { isRecord } from "./map.ts";

const MEMORY_PATH = /(^|[\s"'=/])\.curator(\/|\s|$|["'])/;
const CURATOR_READ = /(^|[\s;&|(])curator\s+(show|list|gaps|search)\b/;

/**
 * True when a tool call reads repository memory: any argument that names the
 * `.curator/` directory (rg, jq, cat, read, grep ...) or a read-only `curator`
 * inspection command. Used only to tell the user; it never changes behavior.
 */
export function isMemoryRecall(input: unknown): boolean {
	if (!isRecord(input)) return false;
	for (const value of Object.values(input)) {
		const strings = typeof value === "string" ? [value] : Array.isArray(value) ? value : [];
		for (const s of strings) {
			if (typeof s === "string" && (MEMORY_PATH.test(s) || CURATOR_READ.test(s))) return true;
		}
	}
	return false;
}
