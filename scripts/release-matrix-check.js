#!/usr/bin/env node
/**
 * Every shipped service must have a published image.
 *
 * `docker-compose.yml` is what a host deploys; `release.yml`'s publish matrix is
 * what it pulls. They were allowed to disagree, and did: `onyx-davd` was a
 * compose service that no release ever published, so the 2026-10-23 v0.5.0
 * deployment pulled twelve images, failed on the thirteenth and had to build the
 * WebDAV daemon on the host to start the stack. Nothing failed in CI, because
 * nothing compared the two lists.
 *
 * This is that comparison. It reads both files and refuses to pass when a service
 * in one is missing from the other — in either direction, because a matrix entry
 * for a service compose does not run publishes an image nobody deploys.
 *
 * A service may be listed in `UNPUBLISHED` with the reason it cannot be
 * published yet. That is a promise the gap is known, not a way to make the check
 * quiet: a service that is not in the matrix and not in `UNPUBLISHED` fails, and
 * a reason goes stale on purpose when somebody reads it.
 *
 *   node scripts/release-matrix-check.js
 */

"use strict";

const fs = require("node:fs");
const path = require("node:path");

const ROOT = path.join(__dirname, "..");
const COMPOSE = "docker-compose.yml";
const MATRIX = path.join(".github", "workflows", "release.yml");

/**
 * Services that exist in compose and have no published image, with the reason.
 *
 * Empty is the goal. An entry here is a known, deliberate gap:
 *
 *  - onyx-davd: the GHCR package `onyx-oss-platform/onyx-davd` already exists
 *    under the innotelinc user but is not linked to this repository, so the
 *    Actions token is refused with "denied: permission_denied: write_package"
 *    and the whole image matrix fails. Link the package to innotelinc/onyx
 *    (package settings → Manage Actions access), add the matrix entry, and
 *    delete this entry.
 */
const UNPUBLISHED = {
  "onyx-davd":
    "its GHCR package is not linked to this repository, so the Actions token's push is denied (write_package)",
};

function read(relative) {
  return fs.readFileSync(path.join(ROOT, relative), "utf8");
}

/** The `onyx-*` services in the compose file's top-level `services:` map. */
function composeServices(source) {
  const lines = source.split(/\r?\n/);
  const start = lines.findIndex((line) => /^services:\s*$/.test(line));
  if (start === -1) throw new Error(`${COMPOSE} has no top-level services: map`);

  const names = [];
  for (const line of lines.slice(start + 1)) {
    // The services map ends where the next top-level key begins.
    if (/^[A-Za-z]/.test(line)) break;
    const match = /^ {2}([A-Za-z0-9][A-Za-z0-9._-]*):\s*$/.exec(line);
    if (match && match[1].startsWith("onyx-")) names.push(match[1]);
  }
  return names;
}

/** The `- service: <name>` entries in release.yml's publish matrix. */
function matrixServices(source) {
  return [...source.matchAll(/^\s*-\s*service:\s*([A-Za-z0-9][A-Za-z0-9._-]*)\s*$/gm)].map((match) => match[1]);
}

function main() {
  const compose = composeServices(read(COMPOSE));
  const matrix = matrixServices(read(MATRIX));

  // A parser that quietly matches nothing would turn this gate off, which is the
  // failure mode it exists to prevent: refuse rather than pass on an empty read.
  if (compose.length < 5) {
    console.error(`release-matrix-check: only ${compose.length} onyx service(s) parsed out of ${COMPOSE} — the file's shape changed, update this check`);
    return 1;
  }
  if (matrix.length < 5) {
    console.error(`release-matrix-check: only ${matrix.length} image(s) parsed out of ${MATRIX} — the matrix moved, update this check`);
    return 1;
  }

  const published = new Set(matrix);
  const declared = new Set(compose);
  const problems = [];

  for (const service of compose) {
    if (published.has(service)) continue;
    if (UNPUBLISHED[service]) {
      console.log(`known gap: ${service} — ${UNPUBLISHED[service]}`);
      continue;
    }
    problems.push(
      `${service} is a compose service with no image in ${MATRIX}'s publish matrix — a host deploying this release cannot pull it`,
    );
  }

  for (const service of matrix) {
    if (!declared.has(service)) {
      problems.push(`${service} is in the publish matrix but not in ${COMPOSE} — the release publishes an image nothing deploys`);
    }
  }

  // An entry for a service that now IS published is a stale promise; say so
  // without failing, because the matrix is right and only the note is old.
  for (const service of Object.keys(UNPUBLISHED)) {
    if (published.has(service)) {
      console.log(`note: ${service} is now published — delete its UNPUBLISHED entry in scripts/release-matrix-check.js`);
    }
  }

  if (problems.length > 0) {
    for (const problem of problems) console.error(`release-matrix-check: ${problem}`);
    return 1;
  }

  console.log(`release-matrix-check: ${compose.length} compose service(s), ${matrix.length} published image(s), no unaccounted gaps`);
  return 0;
}

process.exitCode = main();
