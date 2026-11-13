import { createHash } from "node:crypto";
import { copyFile, readFile, readdir, stat } from "node:fs/promises";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const dist = join(root, "web/admin/dist");
const out = join(root, "web/admin");
const check = process.argv.includes("--check");
const REQUIRED = ["app.css", "app.js", "index.html"];
const EMBED_SHA256 = "6525e4056f999ac8f357dc2a86d004520894d478f4d32e9886971d155e47c0f4";

function fail(message) {
  console.error(`sync-assets: ${message}`);
  process.exit(1);
}

const embed = await readFile(join(out, "embed.go"), "utf8");
const directive = embed.match(/\/\/go:embed\s+([^\n]+)/)?.[1]?.trim();
if (directive !== "index.html app.css app.js") {
  fail(`embed.go must embed exactly "index.html app.css app.js", found "${directive || "none"}"`);
}
const embedHash = createHash("sha256").update(embed.replace(/\r\n/g, "\n")).digest("hex");
if (EMBED_SHA256 !== "PINNED_SHA_PLACEHOLDER" && embedHash !== EMBED_SHA256) {
  fail(`embed.go changed (${embedHash}); the backend must stay untouched`);
}

let entries = [];
try {
  entries = (await readdir(dist)).sort();
} catch {
  fail("web/admin/dist is missing; run vite build first");
}
if (entries.length !== REQUIRED.length || REQUIRED.some((name) => !entries.includes(name))) {
  fail(`dist must contain exactly ${REQUIRED.join(", ")}; found ${entries.join(", ") || "nothing"}`);
}

const css = await readFile(join(dist, "app.css"), "utf8");
if (/@import\s+url\(|url\(\s*['"]?https?:|@font-face/i.test(css)) {
  fail("app.css references a remote or bundled font/image");
}

for (const name of REQUIRED) {
  const file = join(dist, name);
  const info = await stat(file);
  if (!info.isFile() || info.size === 0) fail(`${name} is empty`);
}

if (check) {
  for (const name of REQUIRED) {
    const [built, committed] = await Promise.all([readFile(join(dist, name)), readFile(join(out, name))]);
    if (!built.equals(committed)) fail(`web/admin/${name} is stale; rebuild and commit it`);
  }
  console.log("sync-assets: committed artifacts are current");
} else {
  for (const name of REQUIRED) {
    await copyFile(join(dist, name), join(out, name));
  }
  console.log(`sync-assets: published ${REQUIRED.join(", ")}`);
}
