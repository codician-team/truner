<script setup lang="ts">
import { withBase } from 'vitepress'

const commands = [
  ['truner status', 'health, machines, queue, and today\'s jobs and spend'],
  ['truner doctor', 'end-to-end checks that print a fix for each problem'],
  ['truner jobs --watch', 'live jobs with machine, wait time, duration and cost'],
  ['truner costs --by workflow', 'spend and savings compared with GitHub-hosted runners'],
  ['truner logs 123456789', 'a job machine\'s boot, agent and runner log'],
  ['truner connect 123456789', 'a shell on a job\'s machine over SSM'],
  ['truner explain truner/cpu=8', 'what a label resolves to, with spot prices'],
  ['truner images build ubuntu24-full', 'bake an optimized image ahead of time'],
  ['truner cache stats', 'cache size per repository'],
  ['truner config edit', 'profiles, pools, limits and access rules'],
  ['truner quota --request 512', 'raise EC2 vCPU quotas'],
  ['truner upgrade', 'update the installation to this CLI\'s version'],
]

const checks = [
  'AWS credentials and region',
  'CloudFormation stack and control plane',
  'EC2 vCPU quotas (standard and GPU)',
  'GitHub App permissions, events and installations',
  'Recent webhook deliveries',
  'Recent launch failures',
]
</script>

<template>
  <section class="tl-section ops">
    <div class="tl-container">
      <span class="tl-eyebrow">Day 2</span>
      <h2 class="tl-h2">Run it all from one CLI.</h2>
      <p class="tl-lead">
        One binary installs, upgrades, inspects and removes truner. You never need to open the AWS console for it.
      </p>
      <div class="grid">
        <ul class="cmds">
          <li v-for="c in commands" :key="c[0]">
            <code>{{ c[0] }}</code>
            <span>{{ c[1] }}</span>
          </li>
        </ul>
        <div class="doctor">
          <div class="d-head">
            <code>truner doctor</code>
            <span>checks</span>
          </div>
          <ul>
            <li v-for="c in checks" :key="c">
              <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M5 12.5l4.5 4.5L19 7.5" /></svg>{{ c }}
            </li>
          </ul>
          <p>
            Every problem comes with the command that fixes it. Run several separate installations side by side with
            <code>--stack</code> and <code>--label-prefix</code>.
          </p>
          <a :href="withBase('/cli')">CLI reference →</a>
        </div>
      </div>
    </div>
  </section>
</template>

<style scoped>
.ops {
  background: var(--vp-c-bg-alt);
}

.grid {
  display: grid;
  grid-template-columns: minmax(0, 1.6fr) minmax(0, 1fr);
  gap: 24px;
  margin-top: 48px;
  align-items: start;
}

.cmds {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 10px;
  margin: 0;
  padding: 0;
  list-style: none;
}

.cmds li {
  display: grid;
  gap: 4px;
  padding: 14px 16px;
  border: 1px solid var(--vp-c-divider);
  border-radius: 12px;
  background: var(--vp-c-bg);
}

.cmds code {
  overflow: hidden;
  text-overflow: ellipsis;
  font-family: var(--vp-font-family-mono);
  font-size: 13px;
  font-weight: 500;
  color: var(--vp-c-brand-1);
  white-space: nowrap;
}

.cmds span {
  font-size: 13.5px;
  line-height: 1.5;
  color: var(--vp-c-text-2);
}

.doctor {
  padding: 22px;
  border: 1px solid var(--tl-term-border);
  border-radius: 14px;
  background: var(--tl-term-bg);
  color: var(--tl-term-text);
}

.d-head {
  display: flex;
  align-items: baseline;
  gap: 8px;
}

.d-head code {
  font-family: var(--vp-font-family-mono);
  font-size: 15px;
  font-weight: 600;
  color: var(--tl-term-green);
}

.d-head span {
  color: var(--tl-term-dim);
}

.doctor ul {
  display: grid;
  gap: 8px;
  margin: 16px 0 0;
  padding: 0;
  list-style: none;
  font-size: 14px;
}

.doctor li {
  display: flex;
  align-items: flex-start;
  gap: 8px;
}

.doctor svg {
  flex: none;
  width: 16px;
  height: 16px;
  margin-top: 3px;
  fill: none;
  stroke: var(--tl-term-green);
  stroke-width: 2.5;
  stroke-linecap: round;
  stroke-linejoin: round;
}

.doctor p {
  margin: 18px 0 0;
  font-size: 13.5px;
  line-height: 1.6;
  color: var(--tl-term-dim);
}

.doctor p code {
  font-family: var(--vp-font-family-mono);
  color: var(--tl-term-text);
}

.doctor a {
  display: inline-block;
  margin-top: 14px;
  font-size: 14px;
  font-weight: 600;
  color: var(--tl-term-green);
}

@media (max-width: 960px) {
  .grid {
    grid-template-columns: minmax(0, 1fr);
  }
}

@media (max-width: 560px) {
  .cmds {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
