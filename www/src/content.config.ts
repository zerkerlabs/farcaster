import { docsLoader } from '@astrojs/starlight/loaders';
import { docsSchema } from '@astrojs/starlight/schema';
import { defineCollection } from 'astro:content';

// Starlight's Content Layer loader (the prescribed form since Starlight 0.30 /
// Astro 5) drives the docs collection under src/content/docs/.
export const collections = {
  docs: defineCollection({ loader: docsLoader(), schema: docsSchema() }),
};
