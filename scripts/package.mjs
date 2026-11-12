#!/usr/bin/env node
// Builds the language server for every supported platform and packages each
// into dist/java-navigation-<version>-<platform>.zip, the asset names Lithe
// Store's release tracker recognizes (docs/publishing.md there).
//
// A plugin publishes one archive per platform, and its packaged launch path
// does not vary by architecture (plugin contract v0, "Process start"), so
// each archive carries the one binary for its platform at bin/java-navigation
// (bin/java-navigation.exe on Windows, whose archive's plugin.json names
// that path). The binary is pure Go, cross-compiled without cgo, so every
// archive builds on any machine.
//
// Usage: node scripts/package.mjs [--expect-tag <tag>] [--platform <platform>]

import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { chmodSync, copyFileSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";

const PACKAGE_DIR = "io.github.lithe-idea.java-navigation";
const ASSET_PREFIX = "java-navigation";
const BINARY = "java-navigation";
const TARGETS = [
  { platform: "darwin-arm64", goos: "darwin", goarch: "arm64" },
  { platform: "darwin-x64", goos: "darwin", goarch: "amd64" },
  { platform: "windows-x64", goos: "windows", goarch: "amd64" },
  { platform: "windows-arm64", goos: "windows", goarch: "arm64" },
];
// Files copied next to plugin.json in every archive.
const INCLUDED = ["LICENSE", "THIRD_PARTY_NOTICES.md"];

const manifest = JSON.parse(readFileSync(`${PACKAGE_DIR}/plugin.json`, "utf8"));
const problems = [];
if (manifest.id !== PACKAGE_DIR) problems.push(`plugin.json id ${manifest.id} must equal the directory ${PACKAGE_DIR}`);
if (!/^[0-9]+\.[0-9]+\.[0-9]+$/.test(manifest.version)) problems.push(`version ${manifest.version} is not MAJOR.MINOR.PATCH`);
if (manifest.vendor?.signatureRequirement !== "none") problems.push('vendor.signatureRequirement must be "none"');
if (manifest.id === "dev.lithe" || manifest.id.startsWith("dev.lithe.")) problems.push("dev.lithe.* is reserved for official plugins");
const servers = manifest.languageServers ?? [];
if (servers.length !== 1 || servers[0].launch?.kind !== "packaged" || servers[0].launch?.path !== `bin/${BINARY}`) {
  problems.push(`plugin.json must declare one packaged language server at bin/${BINARY}`);
}

const tagFlag = process.argv.indexOf("--expect-tag");
if (tagFlag >= 0) {
  const tag = process.argv[tagFlag + 1];
  if (tag !== `v${manifest.version}`) problems.push(`tag ${tag} does not match plugin.json version ${manifest.version} (expected v${manifest.version})`);
}
const platformFlag = process.argv.indexOf("--platform");
const targets = platformFlag >= 0 ? TARGETS.filter((t) => t.platform === process.argv[platformFlag + 1]) : TARGETS;
if (targets.length === 0) problems.push(`unknown platform ${process.argv[platformFlag + 1]}`);
if (problems.length > 0) {
  for (const problem of problems) console.error(`✗ ${problem}`);
  process.exit(1);
}

rmSync("dist", { recursive: true, force: true });
mkdirSync("dist", { recursive: true });
const stagingRoot = resolve(".cache", "package");
rmSync(stagingRoot, { recursive: true, force: true });

for (const target of targets) {
  const staging = join(stagingRoot, target.platform);
  const pkg = join(staging, PACKAGE_DIR);
  mkdirSync(join(pkg, "bin"), { recursive: true });
  const exe = target.goos === "windows" ? `${BINARY}.exe` : BINARY;
  execFileSync(
    "go",
    ["build", "-trimpath", "-ldflags", `-s -w -buildid= -X main.version=${manifest.version}`, "-o", join(pkg, "bin", exe), "./cmd/java-navigation"],
    { stdio: "inherit", env: { ...process.env, CGO_ENABLED: "0", GOOS: target.goos, GOARCH: target.goarch } },
  );
  chmodSync(join(pkg, "bin", exe), 0o755);
  const platformManifest = structuredClone(manifest);
  platformManifest.languageServers[0].launch.path = `bin/${exe}`;
  writeFileSync(join(pkg, "plugin.json"), `${JSON.stringify(platformManifest, null, 2)}\n`);
  for (const file of INCLUDED) copyFileSync(file, join(pkg, file));

  const name = `${ASSET_PREFIX}-${manifest.version}-${target.platform}.zip`;
  const asset = resolve("dist", name);
  // -X drops extra file attributes; -y stores a symbolic link as a link so
  // Lithe refuses it instead of the archive silently carrying its target.
  execFileSync("zip", ["-qrXy", asset, PACKAGE_DIR, "-x", "*.DS_Store"], { cwd: staging });
  const sha256 = createHash("sha256").update(readFileSync(asset)).digest("hex");
  console.log(`dist/${name} ${sha256}`);
}
