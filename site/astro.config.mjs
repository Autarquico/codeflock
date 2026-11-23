import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import { BASE, REPO_URL, SITE, GUIDE_PAGES, slugOf } from './scripts/sync-docs.mjs';

export default defineConfig({
  site: SITE,
  base: BASE,
  trailingSlash: 'always',
  integrations: [
    starlight({
      title: 'Code Flock',
      description: 'Coding agents on a network of your own machines, with no server.',
      components: { Search: './src/components/Search.astro' },
      customCss: ['./src/styles/fonts.css', './src/styles/theme.css'],
      expressiveCode: { defaultProps: { wrap: true, preserveIndent: true }, styleOverrides: { borderRadius: '0', codeFontFamily: 'var(--sl-font-mono)' } },
      social: [{ icon: 'github', label: 'GitHub', href: REPO_URL }],
      sidebar: [{ label: 'Guide', items: GUIDE_PAGES.map((file) => ({ slug: slugOf(file) })) }],
    }),
  ],
});
