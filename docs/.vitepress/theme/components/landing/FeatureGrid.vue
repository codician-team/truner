<script setup lang="ts">
import { withBase } from 'vitepress'

// Icons are 24×24 stroke paths.
const features = [
  {
    wide: true,
    icon: 'M4 7c0-1.7 3.6-3 8-3s8 1.3 8 3-3.6 3-8 3-8-1.3-8-3Zm0 0v10c0 1.7 3.6 3 8 3s8-1.3 8-3V7M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3',
    title: 'The magic cache',
    body: '`actions/cache`, every `setup-*` action and `docker buildx` with `type=gha` store their caches in your own S3 bucket. You don\'t change any workflows. There is no size limit, downloads run in parallel inside the region, and pull requests can\'t overwrite the cache of `main`.',
    link: '/caching',
  },
  {
    wide: true,
    icon: 'M12 2v20M17 6.5c-.8-1.5-2.6-2.5-5-2.5-3 0-5 1.4-5 3.6 0 5 10 2.6 10 8 0 2.2-2.2 3.9-5 3.9-2.6 0-4.5-1.1-5.3-2.9',
    title: 'Spot prices, without spot pain',
    body: 'Each request goes to EC2 Fleet with `price-capacity-optimized` across every matching instance type and availability zone, so spot capacity is almost always there. When it isn\'t, the same request falls back to on-demand. Jobs that lose their machine to a spot interruption are re-run on on-demand.',
    link: '/architecture',
  },
  {
    icon: 'M12 3l8 4.5v9L12 21l-8-4.5v-9L12 3Zm0 0v18M4 7.5l8 4.5 8-4.5',
    title: 'A fresh VM for every job',
    body: 'Each runner registration works once and only for its own repository. The machine runs one job and is terminated afterwards, so nothing carries over from one job to the next.',
    link: '/security',
  },
  {
    icon: 'M9 3v3m6-3v3M9 18v3m6-3v3M3 9h3m-3 6h3m12-6h3m-3 6h3M6 6h12v12H6zM10 10h4v4h-4z',
    title: 'x64, Arm64, GPU, Windows',
    body: 'Graviton, NVIDIA GPUs with drivers already installed, local NVMe, up to 96 vCPUs and Windows Server (beta) all use the same label syntax.',
    link: '/images',
  },
  {
    icon: 'M13 2L4 14h7l-1 8 9-12h-7l1-8Z',
    title: 'Fast starts',
    body: 'The first time you use an image, truner bakes an optimized AMI of it and rebuilds it every week. For the fastest starts, keep a warm pool of stopped machines that cost only their disk.',
    link: '/pools',
  },
  {
    icon: 'M3 12h4l3-8 4 16 3-8h4',
    title: 'Guard rails',
    body: 'Set a cap on concurrent instances and a hard monthly budget, and get email alerts. Every job has a timeout and an idle shutdown. `truner costs` shows your spend next to what GitHub-hosted runners would have cost.',
    link: '/costs',
  },
  {
    icon: 'M4 17l6-6-6-6M12 19h8',
    title: 'Debug access built in',
    body: '`truner connect <job>` opens a shell over SSM with no open ports. Or install the triggering user\'s GitHub SSH keys, and add `keep=30m` to look around after the job.',
    link: '/networking',
  },
  {
    icon: 'M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18Zm-9 9h18M12 3c2.5 2.5 3.8 5.5 3.8 9s-1.3 6.5-3.8 9c-2.5-2.5-3.8-5.5-3.8-9S9.5 5.5 12 3Z',
    title: 'Your network, your IAM',
    body: 'Use a dedicated VPC or your own subnets. Add a NAT gateway for a static egress IP and attach IAM policies to the machine role. Everything runs in your AWS account.',
    link: '/networking',
  },
]

function md(s: string) {
  return s.replace(/[&<>]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;' })[c]!).replace(/`([^`]+)`/g, '<code>$1</code>')
}
</script>

<template>
  <section class="tl-section">
    <div class="tl-container">
      <span class="tl-eyebrow">Features</span>
      <h2 class="tl-h2">Everything a CI fleet needs, and nothing to babysit.</h2>
      <p class="tl-lead">
        The control plane is serverless and lives in your account. Machines exist only while they run a job.
      </p>
      <div class="grid">
        <a v-for="f in features" :key="f.title" class="card" :class="{ wide: f.wide }" :href="withBase(f.link)">
          <span class="icon">
            <svg viewBox="0 0 24 24" aria-hidden="true"><path :d="f.icon" /></svg>
          </span>
          <h3>{{ f.title }}</h3>
          <p v-html="md(f.body)" />
        </a>
      </div>
    </div>
  </section>
</template>

<style scoped>
.grid {
  display: grid;
  grid-template-columns: repeat(6, minmax(0, 1fr));
  gap: 16px;
  margin-top: 48px;
}

.card {
  grid-column: span 2;
  display: block;
  padding: 24px;
  border: 1px solid var(--tl-card-border);
  border-radius: 14px;
  background: var(--tl-card-bg);
  transition:
    border-color 0.25s,
    transform 0.25s;
}

.card.wide {
  grid-column: span 3;
}

.card:hover {
  border-color: var(--vp-c-brand-1);
  transform: translateY(-2px);
}

.icon {
  display: grid;
  place-items: center;
  width: 40px;
  height: 40px;
  border-radius: 10px;
  background: var(--vp-c-brand-soft);
  color: var(--vp-c-brand-1);
}

.icon svg {
  width: 22px;
  height: 22px;
  fill: none;
  stroke: currentColor;
  stroke-width: 1.8;
  stroke-linecap: round;
  stroke-linejoin: round;
}

h3 {
  margin: 16px 0 0;
  font-size: 17px;
  font-weight: 600;
  line-height: 1.35;
  color: var(--vp-c-text-1);
}

p {
  margin: 8px 0 0;
  font-size: 14.5px;
  line-height: 1.6;
  color: var(--vp-c-text-2);
}

p :deep(code) {
  font-family: var(--vp-font-family-mono);
  font-size: 0.86em;
  padding: 1px 5px;
  border-radius: 4px;
  background: var(--vp-c-default-soft);
  color: var(--vp-c-text-1);
}

@media (prefers-reduced-motion: reduce) {
  .card:hover {
    transform: none;
  }
}

@media (max-width: 960px) {
  .card {
    grid-column: span 3;
  }
  .card.wide {
    grid-column: span 6;
  }
}

@media (max-width: 640px) {
  .grid {
    margin-top: 32px;
  }
  .card,
  .card.wide {
    grid-column: span 6;
  }
}
</style>
