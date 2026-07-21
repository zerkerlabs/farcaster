import starlight from '@astrojs/starlight';
import { defineConfig } from 'astro/config';
import starlightOpenAPI, { openAPISidebarGroups } from 'starlight-openapi';

// Landing page ("/") is a plain Astro page in src/pages/. Docs live under
// src/content/docs/docs/ (note the doubled "docs" segment) so Starlight's
// folder-driven routing places every doc under /docs/ instead of claiming the
// site root — the "one app, embedded /docs" shape.
export default defineConfig({
  // Production URL — enables the sitemap (Starlight bundles @astrojs/sitemap,
  // which no-ops without `site`) and makes canonical/og/sitemap links absolute.
  // Update this if the site moves to a custom domain.
  site: 'https://farcaster-jade.vercel.app',
  integrations: [
    starlight({
      title: 'Farcaster',
      description:
        'Sovereign, single-binary gateway for agentic traffic — docs and reference.',
      social: [
        { icon: 'github', label: 'GitHub', href: 'https://github.com/zerkerlabs/farcaster' },
      ],
      // The x402 wire-contract API reference is rendered from the canonical
      // x402types/openapi.yaml — the same schema the Go types and the TS SDK
      // generate from — so the docs are a third consumer with zero
      // hand-maintenance. starlight-openapi only
      // renders *operations*, and the canonical file is schema-only
      // (paths: {}), so we point it at a thin wrapper that adds the x402
      // exchange's operation envelope and $refs every field back to the
      // canonical schemas — no schema is copied; a field change there
      // re-renders here untouched. See src/openapi/x402-reference.yaml. Pages
      // land under /docs/api-reference/x402/; the generated nav is spliced
      // into the "API reference" sidebar section below via openAPISidebarGroups.
      //
      // The gateway /v1 REST reference is the second entry:
      // gateway/openapi.yaml IS a full operation document, so
      // starlight-openapi renders it directly — no wrapper. Both references'
      // generated nav groups arrive together via the single openAPISidebarGroups
      // placeholder spliced into the "API reference" sidebar section below.
      plugins: [
        starlightOpenAPI([
          {
            base: 'docs/api-reference/x402',
            label: 'x402 wire contract',
            schema: 'src/openapi/x402-reference.yaml',
          },
          {
            base: 'docs/api-reference/gateway',
            label: 'Gateway REST API',
            schema: '../gateway/openapi.yaml',
          },
        ]),
      ],
      // Explicit (not autogenerate) so the nav order is fixed and intentional
      // — content depth varies by section (Start here, Concepts, Gateway,
      // Payments, and Facilitator are fully written; the rest are shell stubs).
      sidebar: [
        {
          label: 'Start here',
          items: [
            { label: 'What is Farcaster', slug: 'docs' },
            { label: 'Install', slug: 'docs/install' },
            { label: 'Quickstart', slug: 'docs/quickstart' },
            { label: 'OSS vs Commercial at a glance', slug: 'docs/oss-vs-commercial' },
          ],
        },
        {
          label: 'Concepts',
          items: [
            { label: 'Architecture', slug: 'docs/concepts/architecture' },
            { label: 'Sovereignty & no-custody', slug: 'docs/concepts/sovereignty' },
            { label: 'Auth & multi-tenancy', slug: 'docs/concepts/auth-and-multi-tenancy' },
            { label: 'The open-core boundary', slug: 'docs/concepts/open-core-boundary' },
          ],
        },
        {
          label: 'Gateway',
          items: [
            { label: 'Overview', slug: 'docs/gateway' },
            { label: 'Agent Catalog', slug: 'docs/gateway/catalog' },
            { label: 'Routing & proxy', slug: 'docs/gateway/proxy' },
            { label: 'MCP-native transport', slug: 'docs/gateway/mcp' },
            { label: 'Observability & analytics', slug: 'docs/gateway/observability' },
          ],
        },
        {
          label: 'Payments (x402)',
          items: [
            { label: 'Overview', slug: 'docs/payments' },
            { label: 'The wire contract', slug: 'docs/payments/wire-contract' },
            { label: 'Gate vs settle', slug: 'docs/payments/gate-vs-settle' },
          ],
        },
        {
          label: 'Facilitator',
          items: [
            { label: 'Overview', slug: 'docs/facilitator' },
            { label: 'Custody posture', slug: 'docs/facilitator/custody' },
            { label: 'Self-hosting vs managed', slug: 'docs/facilitator/self-hosting' },
            { label: 'Endpoints', slug: 'docs/facilitator/endpoints' },
            { label: 'Signer backends', slug: 'docs/facilitator/signers' },
          ],
        },
        {
          label: 'SDKs',
          items: [{ label: 'Overview', slug: 'docs/sdks' }],
        },
        {
          label: 'API reference',
          items: [
            { label: 'Overview', slug: 'docs/api-reference' },
            // Generated by starlight-openapi from x402types/openapi.yaml.
            ...openAPISidebarGroups,
          ],
        },
        {
          label: 'Self-hosting & operations',
          items: [
            { label: 'Overview', slug: 'docs/self-hosting' },
            { label: 'Deployment', slug: 'docs/self-hosting/deployment' },
            { label: 'Configuration reference', slug: 'docs/self-hosting/configuration' },
            { label: 'Postgres', slug: 'docs/self-hosting/postgres' },
            { label: 'KMS & secrets', slug: 'docs/self-hosting/kms-and-secrets' },
            { label: 'Upgrades', slug: 'docs/self-hosting/upgrades' },
          ],
        },
        {
          label: 'Commercial',
          items: [{ label: 'Overview', slug: 'docs/commercial' }],
        },
      ],
    }),
  ],
});
