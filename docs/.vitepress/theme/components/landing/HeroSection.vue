<script setup lang="ts">
import { withBase } from 'vitepress'
import InstallCommand from './InstallCommand.vue'
import TerminalWindow from './TerminalWindow.vue'

type Line = { kind: 'step' | 'ok' | 'info' | 'ready'; text: string; dim?: string; pause?: number }

// What `truner install` prints (internal/cli/install.go), with example values.
const lines: Line[] = [
  { kind: 'step', text: '[1/5] AWS account' },
  { kind: 'ok', text: 'Account 123456789012 as ', dim: 'arn:aws:iam::123456789012:user/you' },
  { kind: 'ok', text: 'Region us-east-1, installation "truner"' },
  { kind: 'step', text: '[2/5] Preflight checks', pause: 250 },
  { kind: 'ok', text: '6 availability zones (us-east-1a, us-east-1b, …)' },
  { kind: 'ok', text: 'EC2 vCPU quotas: 640 spot, 640 on-demand' },
  { kind: 'step', text: '[3/5] Control plane', pause: 250 },
  { kind: 'ok', text: 'Uploaded control plane and agent artifacts' },
  { kind: 'info', text: 'Deploying CloudFormation stack "truner" (takes 2-4 minutes)...' },
  { kind: 'ok', text: 'Control plane ready in 3m04s', pause: 900 },
  { kind: 'step', text: '[4/5] GitHub App', pause: 250 },
  { kind: 'ok', text: 'Created GitHub App truner-acme', pause: 500 },
  { kind: 'ok', text: 'Installed on acme (all repositories)', pause: 500 },
  { kind: 'step', text: '[5/5] Finishing', pause: 250 },
  { kind: 'ok', text: 'Control plane is healthy (version v0.1.0)' },
  { kind: 'ok', text: 'Building optimized ubuntu24 images (x64, arm64) in the background' },
  { kind: 'ready', text: 'truner is ready!', pause: 400 },
]

// Cumulative reveal times for the CSS animation (no JavaScript needed).
let t = 1300
const timed = lines.map((l) => {
  t += (l.pause ?? 0) + 170
  return { ...l, delay: t }
})

const proof = ['≈ $0 while idle', 'x64 · Arm64 · GPU · Windows', 'No license key, no telemetry']
</script>

<template>
  <section class="hero">
    <div class="backdrop" aria-hidden="true" />
    <div class="tl-container grid">
      <div class="copy">
        <a class="pill" href="https://github.com/truner/truner">
          <span class="pill-tag">Open source</span>
          <span>MIT licensed<span class="pill-more"> · runs in your AWS account</span></span>
        </a>
        <h1>
          Self-hosted runners,
          <span class="tl-gradient-text">minus the self&#8209;hosting.</span>
        </h1>
        <p class="sub">
          truner gives every GitHub Actions job a fresh EC2 machine in your own AWS account. It picks the
          cheapest spot machine that fits, caches to S3 with no size limit and fixes most failures on its own.
          One command to install, one line to switch, and next to nothing to pay while it's idle.
        </p>
        <div class="actions">
          <a class="btn brand" :href="withBase('/getting-started')">Get started</a>
          <a class="btn alt" href="https://github.com/truner/truner">
            <svg viewBox="0 0 24 24" aria-hidden="true">
              <path
                d="M12 .5a11.5 11.5 0 0 0-3.64 22.41c.58.1.79-.25.79-.56v-2c-3.2.7-3.88-1.37-3.88-1.37-.52-1.33-1.28-1.69-1.28-1.69-1.04-.71.08-.7.08-.7 1.15.08 1.76 1.19 1.76 1.19 1.03 1.76 2.69 1.25 3.35.96.1-.75.4-1.25.73-1.54-2.55-.29-5.24-1.28-5.24-5.69 0-1.26.45-2.29 1.19-3.09-.12-.29-.52-1.46.11-3.05 0 0 .97-.31 3.17 1.18a11 11 0 0 1 5.77 0c2.2-1.49 3.17-1.18 3.17-1.18.63 1.59.23 2.76.11 3.05.74.8 1.19 1.83 1.19 3.09 0 4.42-2.69 5.39-5.26 5.68.41.36.78 1.06.78 2.14v3.17c0 .31.21.67.8.56A11.5 11.5 0 0 0 12 .5Z"
              />
            </svg>
            View on GitHub
          </a>
        </div>
        <InstallCommand class="install" />
        <ul class="proof">
          <li v-for="p in proof" :key="p">
            <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M5 12.5l4.5 4.5L19 7.5" /></svg>{{ p }}
          </li>
        </ul>
      </div>

      <div class="demo">
        <TerminalWindow title="~ — truner install">
          <div class="cmd">
            <span class="prompt">$</span>
            <span class="typed">truner install</span>
          </div>
          <div
            v-for="(l, i) in timed"
            :key="i"
            class="ln"
            :class="l.kind"
            :style="{ animationDelay: `${l.delay}ms` }"
          >
            <template v-if="l.kind === 'ok'"><span class="check">✓</span> {{ l.text }}<span v-if="l.dim" class="dim">{{ l.dim }}</span></template>
            <template v-else-if="l.kind === 'step'"><span class="num">{{ l.text.slice(0, 5) }}</span> <b>{{ l.text.slice(6) }}</b></template>
            <template v-else>{{ l.text }}</template>
          </div>
        </TerminalWindow>
      </div>
    </div>
  </section>
</template>

<style scoped>
.hero {
  position: relative;
  overflow: hidden;
  padding: 72px 0 96px;
}

.backdrop {
  position: absolute;
  inset: 0;
  background:
    radial-gradient(60% 50% at 75% 30%, var(--tl-glow), transparent 70%),
    radial-gradient(40% 40% at 10% 0%, var(--tl-glow), transparent 70%),
    linear-gradient(var(--tl-grid-line) 1px, transparent 1px) 0 0 / 48px 48px,
    linear-gradient(90deg, var(--tl-grid-line) 1px, transparent 1px) 0 0 / 48px 48px;
  mask-image: linear-gradient(to bottom, black 55%, transparent);
  pointer-events: none;
}

.grid {
  position: relative;
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(0, 1.02fr);
  gap: 56px;
  align-items: center;
}

.pill {
  display: inline-flex;
  align-items: center;
  gap: 10px;
  padding: 4px 14px 4px 4px;
  border: 1px solid var(--vp-c-divider);
  border-radius: 999px;
  background: var(--vp-c-bg);
  font-size: 13px;
  color: var(--vp-c-text-2);
  transition: border-color 0.2s;
}

.pill:hover {
  border-color: var(--vp-c-brand-1);
}

.pill-tag {
  white-space: nowrap;
  padding: 2px 10px;
  border-radius: 999px;
  background: var(--vp-c-brand-soft);
  font-weight: 600;
  color: var(--vp-c-brand-1);
}

h1 {
  margin: 20px 0 0;
  font-size: clamp(38px, 5.4vw, 60px);
  line-height: 1.05;
  font-weight: 800;
  letter-spacing: -0.035em;
  color: var(--vp-c-text-1);
}

h1 span {
  display: block;
}

.sub {
  margin: 22px 0 0;
  max-width: 560px;
  font-size: 18px;
  line-height: 1.6;
  color: var(--vp-c-text-2);
}

.actions {
  display: flex;
  flex-wrap: wrap;
  gap: 12px;
  margin-top: 30px;
}

.btn {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  height: 44px;
  padding: 0 20px;
  border-radius: 999px;
  font-size: 15px;
  font-weight: 600;
  transition:
    background-color 0.2s,
    border-color 0.2s,
    color 0.2s;
}

.btn.brand {
  background: var(--vp-button-brand-bg);
  color: #fff;
}

.btn.brand:hover {
  background: var(--vp-button-brand-hover-bg);
}

.btn.alt {
  border: 1px solid var(--vp-c-divider);
  background: var(--vp-c-bg);
  color: var(--vp-c-text-1);
}

.btn.alt:hover {
  border-color: var(--vp-c-text-3);
}

.btn svg {
  width: 18px;
  height: 18px;
  fill: currentColor;
}

.install {
  margin-top: 28px;
}

.proof {
  display: flex;
  flex-wrap: wrap;
  gap: 8px 20px;
  margin: 22px 0 0;
  padding: 0;
  list-style: none;
  font-size: 14px;
  color: var(--vp-c-text-2);
}

.proof li {
  display: flex;
  align-items: center;
  gap: 6px;
}

.proof svg {
  width: 16px;
  height: 16px;
  fill: none;
  stroke: var(--vp-c-brand-1);
  stroke-width: 2.5;
  stroke-linecap: round;
  stroke-linejoin: round;
}

/* Terminal replay */
.demo :deep(.term-body) {
  min-height: 452px;
}

.cmd {
  display: flex;
  gap: 10px;
  margin-bottom: 6px;
}

.prompt {
  color: var(--tl-term-green);
}

.typed {
  display: inline-block;
  overflow: hidden;
  width: 0;
  border-right: 2px solid transparent;
  white-space: nowrap;
  animation:
    tl-type 0.8s steps(14) 0.3s forwards,
    tl-caret 0.6s step-end 0s 3;
}

.ln {
  opacity: 0;
  white-space: pre-wrap;
  padding-left: 1.2em;
  text-indent: -1.2em;
  animation: tl-show 0.25s ease forwards;
}

.ln.step {
  margin-top: 8px;
  padding-left: 0;
  text-indent: 0;
}

.ln.ok,
.ln.info {
  margin-left: 1.2em;
}

.num {
  color: var(--tl-term-cyan);
}

.check {
  color: var(--tl-term-green);
}

.dim {
  color: var(--tl-term-dim);
}

.ln.ready {
  margin-top: 12px;
  font-weight: 700;
  color: var(--tl-term-green);
}

@keyframes tl-type {
  to {
    width: 14ch;
  }
}

@keyframes tl-caret {
  50% {
    border-color: var(--tl-term-text);
  }
}

@keyframes tl-show {
  from {
    opacity: 0;
    transform: translateY(3px);
  }
  to {
    opacity: 1;
    transform: none;
  }
}

@media (prefers-reduced-motion: reduce) {
  .typed {
    width: auto;
    animation: none;
  }
  .ln {
    opacity: 1;
    animation: none;
  }
}

@media (max-width: 960px) {
  .grid {
    grid-template-columns: minmax(0, 1fr);
    gap: 48px;
  }
  .demo :deep(.term-body) {
    min-height: 0;
  }
}

@media (max-width: 640px) {
  .hero {
    padding: 40px 0 64px;
  }
  .pill-more {
    display: none;
  }
  .sub {
    font-size: 16px;
  }
}
</style>
