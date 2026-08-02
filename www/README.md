# Farcaster website (`www/`)

The developer documentation site, [Astro](https://astro.build) +
[Starlight](https://starlight.astro.build), deployed to
**docs.farcastergateway.com**. The repo's only Node/JS toolchain — it sits
outside the Go workspace (`go.work`).

The product-facing marketing site is a separate repo,
[`zerkerlabs/farcastergateway`](https://github.com/zerkerlabs/farcastergateway),
serving farcastergateway.com. This project is docs only.

## Layout

- `src/content/docs/` — the docs tree. Starlight owns the site root, so a page
  at `src/content/docs/<path>` serves at `/<path>/`: "What is Farcaster" is `/`
  and Quickstart is `/quickstart/`. Some sections are still shell stubs.
- `vercel.json` — forwards the old `/docs/...` URLs to their new root-level
  homes, so links published before the move keep resolving.
- `src/components/Commercial.astro` — the reusable inline badge marking
  managed-tier material, used across the docs. Only usable from `.mdx` content
  files (Starlight bundles MDX support automatically).
- `astro.config.mjs` — the Starlight integration config (title, sidebar). The
  sidebar is hand-written rather than `autogenerate`d so the nav order is
  explicit.

## Local development

```sh
npm install
npm run dev      # http://localhost:4321
npm run build    # -> dist/
npm run preview  # serve the built output locally
```

## Quality gate

`make check` (mirrors the Go modules' per-module gate) runs:

1. `astro check` — type/content diagnostics (`lint`).
2. `astro build` — the static build (`build`).
3. `linkinator` against the built output, internal links only — external
   `http(s)` links are skipped to keep the gate hermetic (`check-links`).

CI runs this as the `check-www` context (`.github/workflows/ci.yml`),
path-aware and always-reporting — it stays green (short-circuits) when `www/`
is untouched, so it never deadlocks a merge.

**CI drift-guard.** The API reference renders live from `x402types/openapi.yaml`
and `gateway/openapi.yaml` at build time — no generated file is committed, so
nothing needs hand-regenerating when a field or endpoint changes. What *can*
drift silently is the build itself never running: `check-www`'s path list also
watches those two files (not just `www/`), so a schema change on a branch that
touches neither `www/` nor the schema's own module still triggers the full
`astro build` and fails loudly if the rendered reference doesn't build cleanly
against it.

## Deploy

Static output, deployed to Vercel (`vercel.json`, committed). Nothing
Vercel-specific ships in `dist/` — the build is a plain static site, deployable
to any static host.

The canonical/OG/sitemap base URL comes from a single source of truth: `site`
in `astro.config.mjs`. Update it there — and nowhere else — when the site moves
to a custom domain.

## Dependencies

`package-lock.json` is committed, so installs are reproducible (the gate runs
`npm ci`). The site tracks the Astro-7 line (`astro@^7.0.2` +
`@astrojs/starlight@^0.41` + `starlight-openapi@0.26`); this is a public,
internet-facing site, so it must stay off versions with open advisories.

**Run `make audit` (`npm audit --audit-level=high`) when you touch
dependencies** — before opening a PR that changes `package.json` or
`package-lock.json`, and periodically as upstream advisories are published. It
is a **PR checklist item, deliberately not part of `make check`**: `npm audit`
queries the registry advisory DB over the network, and a newly-published
advisory against an unrelated transitive dep must not fail the hermetic build
gate (or every open PR at once). Keep the audit clean, or record the residual:

- `overrides` in `package.json` pins patched transitive build-time deps
  (`form-data`, `uuid`) that the tooling — `starlight-openapi`'s code-sample
  generator and `linkinator` — pull in. The site is static, so neither
  vulnerable path is reachable at runtime, but we keep the audit clean rather
  than carry residual advisories.
