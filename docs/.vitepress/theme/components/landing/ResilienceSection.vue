<script setup lang="ts">
import { withBase } from 'vitepress'

const failures = [
  ['A webhook delivery fails', 'The janitor redelivers it through the GitHub App API.'],
  ['There is no spot capacity', 'The same request falls back to on-demand.'],
  ['Spot interrupts a running job', 'The job is re-run on on-demand capacity once the workflow run finishes.'],
  ['A machine dies before it picks up its job', 'A new machine is launched on on-demand capacity.'],
  ['The vCPU quota runs out', 'Launches are retried with backoff, and you get an email alert.'],
  ['A label or config is invalid', 'The job fails right away and its log explains why. Nothing stays queued.'],
  ['The monthly budget is reached', 'Jobs wait in the queue and you get an alert.'],
]

// The banner the agent prints at the top of every job (internal/agent/flow.go), with example values.
const banner = [
  ['Machine:', 'm7a.xlarge (spot) i-0a1b2c3d4e5f67890 in us-east-1b'],
  ['Requested:', '4 vCPU / 16+ GiB linux x64, spot'],
  ['Image:', 'ubuntu24 (baked image ami-0f3c…), actions/runner 2.338.2, truner v0.1.0'],
  ['Cache:', 'truner magic cache (S3 bucket truner-data-…) for actions/cache, setup-* caching and buildx type=gha'],
  ['Boot:', 'machine 18s, agent 3.1s [prepare 0.4s, docker 1.3s, ecr-login 0.8s]'],
]
</script>

<template>
  <section class="tl-section">
    <div class="tl-container">
      <span class="tl-eyebrow">Self-healing</span>
      <h2 class="tl-h2">Built to recover without you.</h2>
      <p class="tl-lead">
        Webhooks get lost, spot capacity disappears and quotas run out. truner expects all of that and handles it on
        its own, so a job never sits in the queue forever.
      </p>

      <div class="grid">
        <ul class="failures">
          <li v-for="f in failures" :key="f[0]">
            <span class="when">{{ f[0] }}</span>
            <span class="then">{{ f[1] }}</span>
          </li>
        </ul>

        <div class="log" aria-label="Example job log">
          <div class="log-head">
            <span class="status" aria-hidden="true">✓</span>
            <b>test</b>
            <span class="muted">Example job log · succeeded in 2m 41s</span>
          </div>
          <div class="log-body">
            <div class="stepname open">Set up runner</div>
            <div class="out strong">
              truner | m7a.xlarge spot in us-east-1b | 4 vCPU, 15 GiB | ready 21s after boot, 33s after the job was
              queued
            </div>
            <div class="out group">▾ truner runner details</div>
            <div v-for="b in banner" :key="b[0]" class="out kv">
              <span>{{ b[0] }}</span><span>{{ b[1] }}</span>
            </div>
            <div class="stepname">Run actions/checkout@v7</div>
            <div class="stepname">Run actions/setup-node@v7</div>
            <div class="stepname">Run npm ci &amp;&amp; npm test</div>
            <div class="stepname open">Complete runner</div>
            <div class="out strong">
              truner magic cache: 2 hit(s), 0 miss(es), 1 save(s) · restored 412.0 MiB at 588.3 MiB/s · saved 96.4 MiB
            </div>
          </div>
        </div>
      </div>
      <p class="foot">
        Every job log starts with where it ran and how long the boot took, and ends with a cache summary.
        <a :href="withBase('/troubleshooting')">Troubleshooting →</a>
      </p>
    </div>
  </section>
</template>

<style scoped>
.grid {
  display: grid;
  grid-template-columns: minmax(0, 0.9fr) minmax(0, 1.1fr);
  gap: 48px;
  align-items: start;
  margin-top: 48px;
}

.failures {
  margin: 0;
  padding: 0;
  list-style: none;
}

.failures li {
  display: grid;
  gap: 2px;
  padding: 14px 0 14px 18px;
  border-left: 2px solid var(--vp-c-divider);
  transition: border-color 0.2s;
}

.failures li:hover {
  border-color: var(--vp-c-brand-1);
}

.when {
  font-weight: 600;
  color: var(--vp-c-text-1);
}

.then {
  font-size: 14.5px;
  line-height: 1.55;
  color: var(--vp-c-text-2);
}

.log {
  overflow: hidden;
  border: 1px solid var(--tl-term-border);
  border-radius: 12px;
  background: var(--tl-term-bg);
  box-shadow: 0 24px 48px -20px rgba(2, 6, 23, 0.45);
  color: var(--tl-term-text);
}

.log-head {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 12px 16px;
  border-bottom: 1px solid var(--tl-term-border);
  background: var(--tl-term-bar);
  font-size: 14px;
}

.status {
  display: grid;
  place-items: center;
  width: 18px;
  height: 18px;
  border-radius: 50%;
  background: #16a34a;
  font-size: 11px;
  font-weight: 700;
  color: #fff;
}

.muted {
  margin-left: auto;
  font-size: 12px;
  color: var(--tl-term-dim);
  white-space: nowrap;
}

.log-body {
  padding: 10px 0 14px;
  overflow-x: auto;
  font-family: var(--vp-font-family-mono);
  font-size: 12.5px;
  line-height: 1.7;
}

.stepname {
  padding: 3px 16px;
  font-family: var(--vp-font-family-base);
  font-size: 13.5px;
  color: var(--tl-term-text);
}

.stepname::before {
  content: '▸';
  display: inline-block;
  width: 18px;
  color: var(--tl-term-dim);
}

.stepname.open::before {
  content: '▾';
}

.out {
  padding: 0 16px 0 52px;
  color: var(--tl-term-dim);
}

.out.strong {
  color: var(--tl-term-green);
}

.out.group {
  color: var(--tl-term-text);
}

.kv {
  display: flex;
  gap: 1ch;
  padding-left: calc(52px + 2ch);
}

.kv span:first-child {
  flex: none;
  width: 11ch;
}

.foot {
  margin: 28px 0 0;
  font-size: 14px;
  color: var(--vp-c-text-2);
}

.foot a {
  font-weight: 600;
  color: var(--vp-c-brand-1);
}

@media (max-width: 960px) {
  .grid {
    grid-template-columns: minmax(0, 1fr);
    gap: 32px;
  }
}

@media (max-width: 640px) {
  .out {
    padding-left: 34px;
  }
  .kv {
    padding-left: 34px;
  }
  .kv span:first-child {
    width: 10ch;
  }
  .muted {
    display: none;
  }
}
</style>
