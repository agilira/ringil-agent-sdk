import { spawn, spawnSync } from "node:child_process";
import {
  mkdtempSync,
  writeFileSync,
  readFileSync,
  mkdirSync,
  existsSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { resolve, join } from "node:path";
import { fileURLToPath } from "node:url";
const root = resolve(fileURLToPath(new URL("../..", import.meta.url)));
const envFile = existsSync(join(root, ".env"))
  ? readFileSync(join(root, ".env"), "utf8")
  : "";
const key =
  process.env.NVIDIA_API_KEY ||
  envFile
    .match(/^\s*(?:export\s+)?NVIDIA_API_KEY\s*=\s*["']?([^\r\n"']+)/m)?.[1]
    ?.trim();
if (!key)
  throw new Error(
    "NVIDIA_API_KEY is required in the server environment or root .env",
  );
const state = mkdtempSync(join(tmpdir(), "ringil-app-"));
mkdirSync(join(state, "config", "ringil"), { recursive: true });
mkdirSync(join(state, "work"));
const result = spawnSync("go", ["build", "-o", join(state, "ringil"), "."], {
  cwd: join(root, "cmd/ringil"),
  stdio: ["ignore", "ignore", "pipe"],
});
if (result.status !== 0) throw new Error("Could not build ringil");
writeFileSync(
  join(state, "config", "ringil", "instructions.md"),
  "For UI acceptance testing: follow user requests precisely. Use write_file when asked to create a file, and create_artifact when asked for an artifact. Do not read credentials, environment variables, or files outside the working directory. Keep responses short.",
);
writeFileSync(
  join(state, "config", "ringil", "ringil.yaml"),
  `models: [nvidia:nvidia/nemotron-3.5-lightning-30b-a3b]\ninstructions: instructions.md\nmax_turns: 8\nserve:\n  listen: 127.0.0.1:18765\n  app: ${join(root, "app", "dist")}\n`,
);
writeFileSync(join(root, "app", ".e2e-state"), state, { mode: 0o600 });
const proc = spawn(join(state, "ringil"), ["serve", "--no-browser"], {
  cwd: join(state, "work"),
  env: {
    ...process.env,
    NVIDIA_API_KEY: key,
    XDG_CONFIG_HOME: join(state, "config"),
    XDG_DATA_HOME: join(state, "data"),
    XDG_CACHE_HOME: join(state, "cache"),
  },
  stdio: ["ignore", "pipe", "pipe"],
});
// Drain server logs without exposing model/tool inputs or environment values.
proc.stdout.resume();
proc.stderr.resume();
proc.on("exit", (code) => process.exit(code ?? 1));
for (const sig of ["SIGTERM", "SIGINT"]) process.on(sig, () => proc.kill(sig));
