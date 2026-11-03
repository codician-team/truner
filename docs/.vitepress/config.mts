import { readFileSync } from 'node:fs'
import { defineConfig } from 'vitepress'

const repo = 'https://github.com/truner/truner'

// The newest release in the changelog, shown in the navigation bar.
const version =
  readFileSync(new URL('../../CHANGELOG.md', import.meta.url), 'utf8').match(/^## (v\S+)/m)?.[1] ?? 'releases'

// GitHub Pages serves project sites from /<repository>/. The Pages workflow sets
// DOCS_BASE; a custom domain or local preview uses the root.
const base = process.env.DOCS_BASE || '/'

export default defineConfig({
  title: 'truner',
  description:
    'Fast, cheap, ephemeral GitHub Actions runners in your own AWS account. Spot pricing, an unlimited S3 cache, warm pools and zero idle cost.',
  base,
  cleanUrls: true,
  srcExclude: ['README.md'],
  head: [
    ['link', { rel: 'icon', type: 'image/svg+xml', href: `${base}logo.svg` }],
    ['meta', { name: 'theme-color', content: '#059669' }],
    ['meta', { property: 'og:type', content: 'website' }],
    ['meta', { property: 'og:site_name', content: 'truner' }],
    ['meta', { property: 'og:title', content: 'truner: GitHub Actions runners in your AWS account' }],
    [
      'meta',
      {
        property: 'og:description',
        content: 'A fresh EC2 machine for every job, spot pricing, an unlimited S3 cache and zero idle cost. Open source.',
      },
    ],
  ],

  themeConfig: {
    logo: { src: '/logo.svg', alt: '' },
    nav: [
      { text: 'Guide', link: '/getting-started', activeMatch: '^/(?!cli|labels)[a-z]' },
      { text: 'Labels', link: '/labels' },
      { text: 'CLI', link: '/cli' },
      {
        text: version,
        items: [
          { text: 'Changelog', link: `${repo}/blob/main/CHANGELOG.md` },
          { text: 'Releases', link: `${repo}/releases` },
          { text: 'Contributing', link: `${repo}/blob/main/CONTRIBUTING.md` },
        ],
      },
    ],
    sidebar: [
      {
        text: 'Introduction',
        items: [
          { text: 'Getting started', link: '/getting-started' },
          { text: 'FAQ', link: '/faq' },
        ],
      },
      {
        text: 'Using truner',
        items: [
          { text: 'Labels', link: '/labels' },
          { text: 'Configuration', link: '/configuration' },
          { text: 'Caching', link: '/caching' },
          { text: 'Images', link: '/images' },
          { text: 'Warm pools', link: '/pools' },
          { text: 'Networking and access', link: '/networking' },
        ],
      },
      {
        text: 'Operating',
        items: [
          { text: 'CLI reference', link: '/cli' },
          { text: 'Costs and limits', link: '/costs' },
          { text: 'Troubleshooting', link: '/troubleshooting' },
          { text: 'Security', link: '/security' },
        ],
      },
      {
        text: 'Internals',
        items: [
          { text: 'Architecture', link: '/architecture' },
          { text: 'Publishing (maintainers)', link: '/publishing' },
        ],
      },
    ],
    socialLinks: [{ icon: 'github', link: repo }],
    search: { provider: 'local' },
    editLink: { pattern: `${repo}/edit/main/docs/:path`, text: 'Edit this page on GitHub' },
    outline: { level: [2, 3] },
    footer: {
      message: 'Released under the MIT License.',
      copyright: 'Not affiliated with GitHub or Amazon Web Services.',
    },
  },

  // Mermaid's chunks are large but only load on pages with diagrams.
  vite: { build: { chunkSizeWarningLimit: 4096 } },

  markdown: {
    config(md) {
      // Inline code such as `${{ github.run_id }}` must not be read as a Vue expression.
      const codeInline = md.renderer.rules.code_inline!
      md.renderer.rules.code_inline = (tokens, idx, options, env, self) => {
        tokens[idx].attrSet('v-pre', '')
        return codeInline(tokens, idx, options, env, self)
      }

      // Render ```mermaid blocks as diagrams (GitHub renders the same blocks natively).
      const fence = md.renderer.rules.fence!
      md.renderer.rules.fence = (tokens, idx, options, env, self) => {
        const token = tokens[idx]
        if (token.info.trim() === 'mermaid') {
          return `<Mermaid code="${encodeURIComponent(token.content)}" />`
        }
        return fence(tokens, idx, options, env, self)
      }
    },
  },
})
