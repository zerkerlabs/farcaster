// Validates vercel.json's shape before it can reach a deploy.
//
// Vercel rejects unknown properties inside redirects/headers/rewrites entries,
// and the deploy fails at *build* time with the site still serving the previous
// build — so nothing in `astro check`, the build, or the link check notices.
// A stray key once shipped to main and silently kept a merged change offline.
// JSON has no comment syntax; rationale belongs in README.md, not in a field.
//
// Deliberately dependency-free and offline: no schema fetch, so CI stays
// deterministic. It checks the failure class that actually bit us — unknown
// properties and missing required ones — not the full Vercel schema.

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const root = join(dirname(fileURLToPath(import.meta.url)), '..');
const TOP = new Set([
  '$schema', 'framework', 'buildCommand', 'installCommand', 'devCommand',
  'outputDirectory', 'ignoreCommand', 'public', 'regions', 'redirects',
  'rewrites', 'headers', 'cleanUrls', 'trailingSlash', 'functions', 'crons',
  'images', 'git',
]);
const REDIRECT = new Set(['source', 'destination', 'permanent', 'statusCode', 'has', 'missing']);
const REWRITE = new Set(['source', 'destination', 'has', 'missing']);
const HEADER = new Set(['source', 'headers', 'has', 'missing']);

const errors = [];
const check = (obj, allowed, where, required = []) => {
  for (const key of Object.keys(obj)) {
    if (!allowed.has(key)) {
      errors.push(`${where}: unknown property "${key}" — Vercel rejects this and the deploy fails`);
    }
  }
  for (const key of required) {
    if (!(key in obj)) errors.push(`${where}: missing required property "${key}"`);
  }
};

let config;
try {
  config = JSON.parse(readFileSync(join(root, 'vercel.json'), 'utf8'));
} catch (err) {
  console.error(`vercel.json is not valid JSON: ${err.message}`);
  process.exit(1);
}

check(config, TOP, 'vercel.json');
(config.redirects ?? []).forEach((r, i) =>
  check(r, REDIRECT, `redirects[${i}]`, ['source', 'destination']));
(config.rewrites ?? []).forEach((r, i) =>
  check(r, REWRITE, `rewrites[${i}]`, ['source', 'destination']));
(config.headers ?? []).forEach((h, i) =>
  check(h, HEADER, `headers[${i}]`, ['source', 'headers']));

// A path-segment wildcard never matches a trailing slash, and Starlight builds
// directory-style URLs that all end in one. Catch the regression rather than
// trusting a comment nobody reads.
(config.redirects ?? []).forEach((r, i) => {
  if (typeof r.source === 'string' && /:[A-Za-z_][A-Za-z0-9_]*\*/.test(r.source)) {
    errors.push(
      `redirects[${i}]: "${r.source}" uses :param* which does not match a trailing slash; use :param(.*)`,
    );
  }
});

if (errors.length) {
  console.error(`vercel.json: ${errors.length} problem(s)`);
  for (const e of errors) console.error(`  · ${e}`);
  process.exit(1);
}
console.log('vercel.json: ok');
