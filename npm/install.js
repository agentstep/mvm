#!/usr/bin/env node

// Postinstall fallback: if optionalDependencies weren't installed
// (e.g., --no-optional), install the platform package on demand.

const { execSync } = require("child_process");
const fs = require("fs");
const path = require("path");
const os = require("os");

const PLATFORMS = {
  "darwin arm64": "@agentstep/mvm-darwin-arm64",
};

const platformKey = `${process.platform} ${os.arch()}`;
const pkg = PLATFORMS[platformKey];
if (!pkg) process.exit(0); // unsupported platform, skip silently

try {
  require.resolve(`${pkg}/bin/mvm`);
  // Already installed via optionalDependencies
} catch {
  console.log(`@agentstep/mvm: installing platform package ${pkg}...`);
  try {
    execSync(`npm install --no-save ${pkg}@${require("./package.json").version}`, {
      cwd: path.dirname(__dirname),
      stdio: "pipe",
    });
  } catch (e) {
    console.error(`@agentstep/mvm: failed to install ${pkg}`);
    console.error(e.message);
    process.exit(1);
  }
}
