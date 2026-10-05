import { readFileSync } from "node:fs";

/** Contents of plugin/VERSION, the single source of truth for the plugin version. */
export const VERSION = readFileSync(new URL("../VERSION", import.meta.url), "utf8").trim();
