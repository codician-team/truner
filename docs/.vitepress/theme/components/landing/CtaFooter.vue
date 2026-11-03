<script setup lang="ts">
import { withBase } from 'vitepress'
import InstallCommand from './InstallCommand.vue'

const repo = 'https://github.com/codician-team/truner'
const columns = [
  {
    title: 'Docs',
    links: [
      ['Getting started', '/getting-started'],
      ['Labels', '/labels'],
      ['Caching', '/caching'],
      ['CLI reference', '/cli'],
      ['Troubleshooting', '/troubleshooting'],
    ],
  },
  {
    title: 'Project',
    links: [
      ['GitHub', repo],
      ['Releases', `${repo}/releases`],
      ['Changelog', `${repo}/blob/main/CHANGELOG.md`],
      ['Security', '/security'],
      ['Contributing', `${repo}/blob/main/CONTRIBUTING.md`],
    ],
  },
]

const href = (l: string) => (l.startsWith('http') ? l : withBase(l))
</script>

<template>
  <section class="cta">
    <div class="tl-container inner">
      <h2 class="tl-h2">Your next CI run could be on your own AWS account in about five minutes.</h2>
      <p class="tl-lead">Install the CLI, run <code>truner install</code>, then change one line in a workflow.</p>
      <InstallCommand class="cmd" />
      <a class="btn" :href="withBase('/getting-started')">Read the getting started guide →</a>
    </div>
  </section>
  <footer class="footer">
    <div class="tl-container cols">
      <div class="brand">
        <img :src="withBase('/logo.svg')" alt="" width="28" height="28" />
        <b>truner</b>
        <p>Ephemeral GitHub Actions runners in your own AWS account. MIT licensed.</p>
      </div>
      <nav v-for="c in columns" :key="c.title" :aria-label="c.title">
        <h3>{{ c.title }}</h3>
        <a v-for="l in c.links" :key="l[0]" :href="href(l[1])">{{ l[0] }}</a>
      </nav>
    </div>
    <div class="tl-container legal">
      Not affiliated with GitHub, Inc. or Amazon Web Services, Inc. GitHub and AWS are trademarks of their respective
      owners.
    </div>
  </footer>
</template>

<style scoped>
.cta {
  position: relative;
  overflow: hidden;
  padding: 104px 0;
  border-top: 1px solid var(--vp-c-divider);
  background:
    radial-gradient(50% 80% at 50% 100%, var(--tl-glow), transparent 70%),
    var(--vp-c-bg);
}

.inner {
  display: flex;
  flex-direction: column;
  align-items: center;
  text-align: center;
}

.inner .tl-lead {
  margin-left: auto;
  margin-right: auto;
}

.tl-lead code {
  font-family: var(--vp-font-family-mono);
  font-size: 0.88em;
  padding: 2px 6px;
  border-radius: 4px;
  background: var(--vp-c-default-soft);
}

.cmd {
  width: 100%;
  margin-top: 32px;
  text-align: left;
}

.btn {
  margin-top: 28px;
  padding: 10px 22px;
  border-radius: 999px;
  background: var(--vp-button-brand-bg);
  font-weight: 600;
  color: #fff;
  transition: background-color 0.2s;
}

.btn:hover {
  background: var(--vp-button-brand-hover-bg);
}

.footer {
  padding: 56px 0 32px;
  border-top: 1px solid var(--vp-c-divider);
  background: var(--vp-c-bg-alt);
}

.cols {
  display: grid;
  grid-template-columns: 2fr 1fr 1fr;
  gap: 32px;
}

.brand {
  display: grid;
  grid-template-columns: auto 1fr;
  align-content: start;
  align-items: center;
  gap: 10px;
}

.brand b {
  font-size: 18px;
  color: var(--vp-c-text-1);
}

.brand p {
  grid-column: 1 / -1;
  margin: 6px 0 0;
  max-width: 320px;
  font-size: 14px;
  line-height: 1.6;
  color: var(--vp-c-text-2);
}

nav {
  display: grid;
  align-content: start;
  gap: 8px;
}

h3 {
  margin: 0 0 4px;
  font-size: 13px;
  font-weight: 600;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--vp-c-text-3);
}

nav a {
  font-size: 14px;
  color: var(--vp-c-text-2);
  transition: color 0.2s;
}

nav a:hover {
  color: var(--vp-c-brand-1);
}

.legal {
  margin-top: 40px;
  font-size: 12.5px;
  color: var(--vp-c-text-3);
}

@media (max-width: 640px) {
  .cta {
    padding: 72px 0;
  }
  .cols {
    grid-template-columns: 1fr 1fr;
  }
  .brand {
    grid-column: 1 / -1;
  }
}
</style>
