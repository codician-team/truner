import { readFileSync, writeFileSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { defineConfigWithTheme, type DefaultTheme } from 'vitepress'

const repo = 'https://github.com/codician-team/truner'

// The newest release in the changelog, shown in the navigation bar.
const version =
  readFileSync(new URL('../../CHANGELOG.md', import.meta.url), 'utf8').match(/^## (v\S+)/m)?.[1] ?? 'releases'

// GitHub Pages serves project sites from /<repository>/. The Pages workflow sets
// DOCS_BASE and DOCS_URL; a custom domain or local preview uses the root.
const base = process.env.DOCS_BASE || '/'
const siteUrl = (process.env.DOCS_URL || 'https://codician-team.github.io/truner').replace(/\/$/, '')

// The star count when the site is built, so the landing page shows it without
// waiting for the GitHub API. The Pages workflow passes GITHUB_TOKEN because
// anonymous API calls from shared runners are often rate limited.
async function fetchStars(): Promise<number | null> {
  try {
    const headers: Record<string, string> = { Accept: 'application/vnd.github+json' }
    if (process.env.GITHUB_TOKEN) headers.Authorization = `Bearer ${process.env.GITHUB_TOKEN}`
    const res = await fetch(repo.replace('github.com', 'api.github.com/repos'), { headers, signal: AbortSignal.timeout(5000) })
    const n = res.ok ? (await res.json()).stargazers_count : null
    return typeof n === 'number' ? n : null
  } catch {
    return null
  }
}
const stars = await fetchStars()

const sidebar: DefaultTheme.SidebarItem[] = [
  {
    text: 'Introduction',
    items: [
      { text: 'Getting started', link: '/getting-started' },
      { text: 'Install with an AI agent', link: '/ai-agents' },
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
      { text: 'MicroVMs (beta)', link: '/microvms' },
      { text: 'Parallel jobs', link: '/parallel-jobs' },
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
]

// One line per page, for search engines and the llms.txt index.
const descriptions: Record<string, string> = {
  '/getting-started': 'Install the CLI, deploy truner to an AWS account and run the first job.',
  '/ai-agents': 'Step-by-step instructions for an AI agent that installs truner for a user.',
  '/faq': 'How truner compares with GitHub-hosted and self-hosted runners, start times, spot interruptions and security.',
  '/labels': 'The runs-on label syntax: CPU, memory, architecture, GPU, image, disk and spot settings, and built-in runner profiles.',
  '/configuration': 'Repository (.github/truner.yml) and account-wide configuration: defaults, runner profiles, images, pools, access and limits.',
  '/caching': 'The S3-backed magic cache for actions/cache and setup-* actions, repository snapshots for actions/checkout, Docker layer caching and local NVMe.',
  '/images': 'Built-in Ubuntu, GPU and Windows images, automatically baked AMIs and custom images.',
  '/pools': 'Warm pools of stopped, prepared machines for faster job starts.',
  '/microvms': 'Run short jobs such as linters and checks on Lambda MicroVMs that resume from a snapshot instead of booting EC2.',
  '/parallel-jobs': 'Splitting one CI job into parallel jobs: measured start and checkout times, run tokens, instance families for timed tests, and passing build output.',
  '/networking': 'VPC options, a static egress IP, debug access over SSM or SSH, and AWS permissions for jobs.',
  '/cli': 'Every truner command and flag.',
  '/costs': 'What truner costs, how to see spend, and budgets, limits and alerts.',
  '/troubleshooting': 'Fixing queued jobs (label prefix, jobs left without a machine), CLI version skew, slow starts and cache misses, and where to find logs.',
  '/security': 'The security model and how to report vulnerabilities.',
  '/architecture': 'How the control plane, agent and CLI work, the job lifecycle and failure handling.',
  '/publishing': 'Releasing truner and publishing it under another GitHub owner.',
}

// Markdown source of a page as an agent should read it: includes resolved,
// front matter removed.
function pageMarkdown(srcDir: string, file: string): string {
  const text = readFileSync(join(srcDir, file), 'utf8')
    .replace(/<!--@include:\s*(.+?)\s*-->/g, (_, p) => readFileSync(resolve(srcDir, dirname(file), p), 'utf8'))
    .replace(/^---\n[\s\S]*?\n---\n/, '')
  return text.trim() + '\n'
}

// Agent-readable docs (https://llmstxt.org): /llms.txt indexes the pages,
// /llms-full.txt holds all of them, and every page is also served as Markdown
// next to its HTML (/labels.md beside /labels).
function writeAgentFiles(srcDir: string, outDir: string) {
  const index = [
    '# truner',
    '',
    '> Fast, cheap, ephemeral GitHub Actions runners in your own AWS account. Every job gets a fresh EC2',
    '> machine (spot with on-demand fallback), caches go to S3 with no size limit, and a serverless',
    '> control plane costs close to nothing while idle. Open source (MIT), installed with one CLI command.',
    '',
    `If you are an AI agent asked to install truner, follow [Install with an AI agent](${siteUrl}/ai-agents.md).`,
    `To change a workflow, read [Labels](${siteUrl}/labels.md). Source code: ${repo}`,
  ]
  const full: string[] = []
  for (const group of sidebar) {
    index.push('', `## ${group.text}`, '')
    for (const item of group.items ?? []) {
      const file = item.link!.slice(1) + '.md'
      const md = pageMarkdown(srcDir, file)
      writeFileSync(join(outDir, file), md)
      index.push(`- [${item.text}](${siteUrl}/${file}): ${descriptions[item.link!]}`)
      full.push(`<!-- ${siteUrl}/${file} -->\n\n${md}`)
    }
  }
  index.push('', '## Optional', '', `- [All pages in one file](${siteUrl}/llms-full.txt)`, '')
  writeFileSync(join(outDir, 'llms.txt'), index.join('\n'))
  writeFileSync(join(outDir, 'llms-full.txt'), full.join('\n\n'))
}

export default defineConfigWithTheme<DefaultTheme.Config & { stars: number | null }>({
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
    stars,
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
    sidebar,
    socialLinks: [{ icon: 'github', link: repo }],
    search: { provider: 'local' },
    editLink: { pattern: `${repo}/edit/main/docs/:path`, text: 'Edit this page on GitHub' },
    outline: { level: [2, 3] },
    footer: {
      message: 'Released under the MIT License.',
      copyright: 'Not affiliated with GitHub or Amazon Web Services.',
    },
  },

  transformPageData(pageData) {
    const desc = descriptions['/' + pageData.relativePath.replace(/\.md$/, '')]
    if (desc && !pageData.frontmatter.description) {
      pageData.description = desc
    }
  },
  // Point agents that fetch a page's HTML at its Markdown copy.
  transformHead({ pageData }) {
    const href = pageData.relativePath === 'index.md' ? `${base}llms.txt` : `${base}${pageData.relativePath}`
    return [['link', { rel: 'alternate', type: 'text/markdown', href }]]
  },
  buildEnd(site) {
    writeAgentFiles(site.srcDir, site.outDir)
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
