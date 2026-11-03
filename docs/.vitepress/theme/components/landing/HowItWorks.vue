<script setup lang="ts">
import { withBase } from 'vitepress'

const steps = [
  {
    title: 'Job queued',
    where: 'GitHub',
    body: 'A job with a truner label is queued. GitHub sends a signed workflow_job webhook to your account.',
  },
  {
    title: 'Spec resolved',
    where: 'Lambda',
    body: 'The control plane verifies the webhook, merges your labels and config into a machine spec, and registers a single-use runner.',
  },
  {
    title: 'Machine launched',
    where: 'EC2 Fleet',
    body: 'It launches the best spot machine across all matching types and AZs, or on-demand if there is no spot capacity.',
  },
  {
    title: 'Job runs',
    where: 'your instance',
    body: 'The agent sets up NVMe, Docker and the cache proxy, then starts the runner, which takes this one job.',
  },
  {
    title: 'Gone',
    where: 'S3 + DynamoDB',
    body: 'The machine powers off and is terminated. Its log goes to S3, and the job\'s cost is recorded.',
  },
]

const janitor = [
  'redelivers failed webhooks',
  'relaunches machines that died before starting',
  'launches a machine again for a job whose machine another job took',
  're-runs spot-interrupted jobs',
  'enforces deadlines and idle timeouts',
  'keeps warm pools full',
  'bakes and prunes images',
  'mirrors the runner into S3',
]
</script>

<template>
  <section class="tl-section how">
    <div class="tl-container">
      <span class="tl-eyebrow">How it works</span>
      <h2 class="tl-h2">From webhook to terminated machine in five steps.</h2>
      <p class="tl-lead">
        There is no truner service and nothing runs between jobs. Everything below runs in your AWS account, mostly
        Lambda functions that cost nothing while idle.
      </p>

      <ol class="flow">
        <li v-for="(s, i) in steps" :key="s.title">
          <div class="node">
            <span class="n">{{ i + 1 }}</span>
            <span class="where">{{ s.where }}</span>
          </div>
          <h3>{{ s.title }}</h3>
          <p>{{ s.body }}</p>
        </li>
      </ol>

      <div class="janitor">
        <div class="j-head">
          <span class="pulse" aria-hidden="true" />
          <b>Every 2 minutes, the janitor</b>
        </div>
        <ul>
          <li v-for="j in janitor" :key="j">{{ j }}</li>
        </ul>
        <a :href="withBase('/architecture')">Architecture →</a>
      </div>
    </div>
  </section>
</template>

<style scoped>
.how {
  background: var(--vp-c-bg-alt);
}

.flow {
  position: relative;
  display: grid;
  grid-template-columns: repeat(5, minmax(0, 1fr));
  gap: 24px;
  margin: 56px 0 0;
  padding: 0;
  list-style: none;
}

.flow::before {
  content: '';
  position: absolute;
  top: 20px;
  left: 20px;
  right: 10%;
  height: 2px;
  background: linear-gradient(90deg, var(--vp-c-brand-1), transparent);
  opacity: 0.45;
}

.flow li {
  position: relative;
}

.node {
  display: flex;
  align-items: center;
  gap: 10px;
}

.n {
  display: grid;
  place-items: center;
  width: 42px;
  height: 42px;
  border: 2px solid var(--vp-c-brand-1);
  border-radius: 50%;
  background: var(--vp-c-bg-alt);
  font-weight: 700;
  color: var(--vp-c-brand-1);
}

.where {
  padding: 2px 8px;
  border-radius: 6px;
  background: var(--vp-c-bg);
  font-family: var(--vp-font-family-mono);
  font-size: 12px;
  color: var(--vp-c-text-2);
  white-space: nowrap;
}

h3 {
  margin: 18px 0 0;
  font-size: 17px;
  font-weight: 600;
  color: var(--vp-c-text-1);
}

.flow p {
  margin: 6px 0 0;
  font-size: 14.5px;
  line-height: 1.6;
  color: var(--vp-c-text-2);
}

.janitor {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 12px 24px;
  margin-top: 48px;
  padding: 18px 22px;
  border: 1px dashed var(--vp-c-divider);
  border-radius: 14px;
  background: var(--vp-c-bg);
  font-size: 14px;
}

.j-head {
  display: flex;
  align-items: center;
  gap: 10px;
  color: var(--vp-c-text-1);
}

.pulse {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--vp-c-brand-1);
  box-shadow: 0 0 0 0 var(--vp-c-brand-1);
  animation: tl-pulse 2s infinite;
}

.janitor ul {
  display: flex;
  flex: 1;
  flex-wrap: wrap;
  gap: 6px;
  margin: 0;
  padding: 0;
  list-style: none;
}

.janitor li {
  padding: 2px 10px;
  border-radius: 999px;
  background: var(--vp-c-default-soft);
  color: var(--vp-c-text-2);
}

.janitor a {
  font-weight: 600;
  color: var(--vp-c-brand-1);
}

@keyframes tl-pulse {
  70% {
    box-shadow: 0 0 0 8px transparent;
  }
}

@media (prefers-reduced-motion: reduce) {
  .pulse {
    animation: none;
  }
}

@media (max-width: 960px) {
  .flow {
    grid-template-columns: minmax(0, 1fr);
    gap: 28px;
  }
  .flow::before {
    top: 20px;
    bottom: 20px;
    left: 20px;
    right: auto;
    width: 2px;
    height: auto;
    background: linear-gradient(180deg, var(--vp-c-brand-1), transparent);
  }
  .flow li {
    padding-left: 60px;
  }
  .node {
    gap: 18px;
    margin-left: -60px;
  }
  h3 {
    margin-top: 8px;
  }
}

@media (max-width: 640px) {
  .janitor {
    flex-direction: column;
    align-items: flex-start;
  }
  .janitor ul {
    flex: none;
  }
}
</style>
