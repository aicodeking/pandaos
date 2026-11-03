// Directory packing for Boat.push and Boat.runJob. Node only, so index.js loads
// it on demand and stays usable in a browser. Uses the system git and tar.
import { execFileSync, spawnSync } from "node:child_process";
import { lstatSync, statSync } from "node:fs";
import { basename, resolve } from "node:path";

const MAX_ARCHIVE = 1 << 30; // above the API's default -max-file-bytes

export function assertDirectory(dir) {
  if (!statSync(dir).isDirectory()) throw new Error(`${dir} is not a directory`);
}

function gitFiles(dir) {
  try {
    const out = execFileSync("git", ["-C", dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard"], { stdio: ["ignore", "pipe", "ignore"], maxBuffer: 1 << 28 });
    // git can track a file deleted here. A submodule is a directory entry: tar takes it whole.
    return [...new Set(out.toString().split("\0").filter(Boolean))].filter((f) => {
      try { lstatSync(resolve(dir, f)); return true; } catch { return false; }
    });
  } catch {
    return null; // not a repository, or no git
  }
}

function tar(dir, args, input) {
  const r = spawnSync("tar", ["-czf", "-", "-C", dir, "--exclude=.git", ...args], { input, maxBuffer: MAX_ARCHIVE, env: { ...process.env, COPYFILE_DISABLE: "1" } });
  if (r.error) throw r.error;
  if (r.status !== 0) throw new Error(`tar: ${r.stderr}`);
  return new Uint8Array(r.stdout);
}

export async function packDirectory(directory) {
  const dir = resolve(directory);
  assertDirectory(dir);
  const listed = gitFiles(dir);
  if (listed) return { name: basename(dir), data: tar(dir, ["--null", "-T", "-"], listed.join("\0") + "\0"), files: listed.length };
  const walked = execFileSync("find", [dir, "-name", ".git", "-prune", "-o", "!", "-type", "d", "-print"], { maxBuffer: 1 << 28 }).toString().split("\n").filter(Boolean);
  return { name: basename(dir), data: tar(dir, ["."]), files: walked.length };
}
