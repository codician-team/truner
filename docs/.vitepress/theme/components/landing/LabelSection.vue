<script setup lang="ts">
import { withBase } from 'vitepress'
import { computed, ref } from 'vue'
import TerminalWindow from './TerminalWindow.vue'

// Output of `truner explain --offline truner/<label>` for v0.1.0.
const examples = [
  {
    label: 'cpu=4',
    note: 'The default shape',
    head: '4 vCPU / 16+ GiB linux x64, spot',
    rows: [['vCPU', '4'], ['memory', '16+ GiB'], ['arch', 'x64'], ['image', 'ubuntu24 (linux, profile standard)'], ['purchase', 'spot, falls back to on-demand'], ['disk', '40 GiB gp3']],
  },
  {
    label: 'runner=8cpu-linux-arm64',
    note: 'Graviton',
    head: '8 vCPU / 32+ GiB linux arm64, spot',
    rows: [['runner', '8cpu-linux-arm64'], ['vCPU', '8'], ['memory', '32+ GiB'], ['arch', 'arm64'], ['image', 'ubuntu24 (linux, profile standard)'], ['purchase', 'spot, falls back to on-demand']],
  },
  {
    label: 'cpu=16/ram=64/spot=false',
    note: 'Big and uninterruptible',
    head: '16 vCPU / 64+ GiB linux x64, on-demand',
    rows: [['vCPU', '16'], ['memory', '64+ GiB'], ['arch', 'x64'], ['image', 'ubuntu24 (linux, profile standard)'], ['purchase', 'on-demand'], ['disk', '40 GiB gp3']],
  },
  {
    label: 'runner=gpu-nvidia',
    note: 'NVIDIA GPU, drivers ready',
    head: '4+ vCPU / 16+ GiB linux x64, 1 GPU, spot',
    rows: [['runner', 'gpu-nvidia'], ['vCPU', '4+'], ['memory', '16+ GiB'], ['GPU', '1'], ['image', 'ubuntu24-gpu (linux, profile standard)'], ['disk', '120 GiB gp3']],
  },
  {
    label: 'cpu=16/nvme=true',
    note: 'Local NVMe for heavy I/O',
    head: '16 vCPU / 64+ GiB linux x64, spot',
    rows: [['vCPU', '16'], ['memory', '64+ GiB'], ['arch', 'x64'], ['image', 'ubuntu24 (linux, profile standard)'], ['purchase', 'spot, falls back to on-demand'], ['disk', '40 GiB gp3 + local NVMe required']],
  },
  {
    label: 'image=ubuntu24-full/cpu=8',
    note: 'GitHub-like toolset',
    head: '8 vCPU / 32+ GiB linux x64, spot',
    rows: [['vCPU', '8'], ['memory', '32+ GiB'], ['arch', 'x64'], ['image', 'ubuntu24-full (linux, profile full)'], ['purchase', 'spot, falls back to on-demand'], ['disk', '40 GiB gp3']],
  },
  {
    label: 'family=c7a+m7a/cpu=4-8',
    note: 'Pick the families',
    head: '4-8 vCPU [c7a*|m7a*] linux x64, spot',
    rows: [['vCPU', '4-8'], ['memory', 'any'], ['families', 'c7a*, m7a*'], ['arch', 'x64'], ['image', 'ubuntu24 (linux, profile standard)'], ['purchase', 'spot, falls back to on-demand']],
  },
  {
    label: 'runner=4cpu-windows-x64',
    note: 'Windows Server (beta)',
    head: '4 vCPU / 16+ GiB windows x64, spot',
    rows: [['runner', '4cpu-windows-x64'], ['vCPU', '4'], ['memory', '16+ GiB'], ['image', 'windows2025 (windows, profile standard)'], ['purchase', 'spot, falls back to on-demand'], ['disk', '80 GiB gp3']],
  },
]

const selected = ref(0)
const ex = computed(() => examples[selected.value])

// [class, indent, key, rest] per line; interpolated text keeps its spaces.
const workflow = computed(() => [
  ['', '', 'jobs', ':'],
  ['', '  ', 'test', ':'],
  ['del', '-   ', 'runs-on', ': ubuntu-latest'],
  ['add', '+   ', 'runs-on', `: truner/${ex.value.label}/run=\${{ github.run_id }}`],
  ['', '    ', 'steps', ':'],
  ['', '      - ', 'uses', ': actions/checkout@v7'],
])
</script>

<template>
  <section class="tl-section">
    <div class="tl-container grid">
      <div>
        <span class="tl-eyebrow">One line to switch</span>
        <h2 class="tl-h2">Ask for resources, not instance types.</h2>
        <p class="tl-lead">
          Change <code>runs-on</code> and you're done. Say how many CPUs, how much memory, which architecture or
          GPU you need. truner asks EC2 for every current-generation type that fits, in every availability zone,
          and takes the best spot offer. If there isn't one, it falls back to on-demand.
        </p>
        <div class="chips" role="group" aria-label="Example labels">
          <button
            v-for="(e, i) in examples"
            :key="e.label"
            :aria-pressed="selected === i"
            :class="{ active: selected === i }"
            @click="selected = i"
          >
            <code>{{ e.label }}</code>
            <span>{{ e.note }}</span>
          </button>
        </div>
        <p class="more">
          <a :href="withBase('/labels')">All labels</a> ·
          <a :href="withBase('/configuration')">Reusable profiles in <code>.github/truner.yml</code></a>
        </p>
      </div>

      <div class="windows">
        <div class="yaml">
          <div class="file">.github/workflows/ci.yml</div>
          <div class="code">
            <div v-for="(l, i) in workflow" :key="i" :class="l[0]">{{ l[1] }}<span class="k">{{ l[2] }}</span>{{ l[3] }}</div>
          </div>
        </div>
        <TerminalWindow title="what will this job get?">
          <div><span class="prompt">$</span> truner explain truner/{{ ex.label }}</div>
          <div class="resolved">Resolved: <b>{{ ex.head }}</b></div>
          <div v-for="r in ex.rows" :key="r[0]" class="row">
            <span class="key">{{ r[0] }}</span><span>{{ r[1] }}</span>
          </div>
          <div class="hint">Without --offline it also lists the matching instance types with live spot prices.</div>
        </TerminalWindow>
      </div>
    </div>
  </section>
</template>

<style scoped>
.grid {
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
  gap: 56px;
  align-items: start;
}

.tl-lead code,
.more code {
  font-family: var(--vp-font-family-mono);
  font-size: 0.88em;
  padding: 2px 6px;
  border-radius: 4px;
  background: var(--vp-c-default-soft);
}

.chips {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 8px;
  margin-top: 28px;
}

.chips button {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 2px;
  padding: 10px 12px;
  border: 1px solid var(--vp-c-divider);
  border-radius: 10px;
  background: var(--vp-c-bg);
  text-align: left;
  transition:
    border-color 0.2s,
    background-color 0.2s;
}

.chips button:hover {
  border-color: var(--vp-c-text-3);
}

.chips button.active {
  border-color: var(--vp-c-brand-1);
  background: var(--vp-c-brand-soft);
}

.chips code {
  max-width: 100%;
  overflow: hidden;
  text-overflow: ellipsis;
  font-family: var(--vp-font-family-mono);
  font-size: 13px;
  color: var(--vp-c-text-1);
  white-space: nowrap;
}

.chips span {
  font-size: 12px;
  color: var(--vp-c-text-2);
}

.more {
  margin: 20px 0 0;
  font-size: 14px;
  color: var(--vp-c-text-3);
}

.more a {
  color: var(--vp-c-brand-1);
  font-weight: 500;
}

.more a:hover {
  text-decoration: underline;
}

.windows {
  display: grid;
  gap: 16px;
}

.yaml {
  overflow: hidden;
  border: 1px solid var(--vp-c-divider);
  border-radius: 12px;
  background: var(--vp-c-bg-alt);
}

.file {
  padding: 8px 16px;
  border-bottom: 1px solid var(--vp-c-divider);
  font-family: var(--vp-font-family-mono);
  font-size: 12px;
  color: var(--vp-c-text-2);
}

.code {
  padding: 14px 0;
  overflow-x: auto;
  font-family: var(--vp-font-family-mono);
  font-size: 13px;
  line-height: 1.7;
  color: var(--vp-c-text-1);
}

.code > div {
  width: max-content;
  min-width: 100%;
  padding: 0 16px;
  white-space: pre;
}

.yaml .k {
  color: var(--vp-c-brand-1);
}

.del {
  background: rgba(239, 68, 68, 0.1);
  color: var(--vp-c-text-2);
  text-decoration: line-through;
  text-decoration-color: rgba(239, 68, 68, 0.5);
}

.add {
  background: rgba(16, 185, 129, 0.12);
  font-weight: 500;
}

.prompt {
  margin-right: 6px;
  color: var(--tl-term-green);
}

.resolved {
  margin: 6px 0 8px;
}

.resolved b {
  color: var(--tl-term-green);
  font-weight: 600;
}

.row {
  display: flex;
  padding-left: 2ch;
}

.key {
  flex: none;
  width: 15ch;
  color: var(--tl-term-dim);
}

.hint {
  margin-top: 10px;
  font-family: var(--vp-font-family-base);
  font-size: 12px;
  color: var(--tl-term-dim);
}

@media (max-width: 960px) {
  .grid {
    grid-template-columns: minmax(0, 1fr);
    gap: 40px;
  }
}

@media (max-width: 480px) {
  .chips {
    grid-template-columns: minmax(0, 1fr);
  }
  .key {
    width: 10ch;
  }
}
</style>
