<script setup lang="ts">
import { withBase } from 'vitepress'
import { computed, ref } from 'vue'

const raw = 'https://raw.githubusercontent.com/codician-team/truner/main'
const tabs = [
  { id: 'unix', label: 'macOS / Linux', prompt: '$', cmd: `curl -fsSL ${raw}/install.sh | sh` },
  { id: 'windows', label: 'Windows', prompt: 'PS>', cmd: `irm ${raw}/install.ps1 | iex` },
  {
    id: 'agent',
    label: 'AI agent',
    prompt: '',
    cmd: 'Install truner in my AWS account so my GitHub Actions jobs run on it.\nFollow https://codician-team.github.io/truner/ai-agents.md',
  },
]

const active = ref(tabs[0].id)
const copied = ref(false)
const current = computed(() => tabs.find((t) => t.id === active.value)!)

async function copy() {
  try {
    await navigator.clipboard.writeText(current.value.cmd)
    copied.value = true
    setTimeout(() => (copied.value = false), 1600)
  } catch {
    // Clipboard access can be blocked; the command stays selectable.
  }
}
</script>

<template>
  <div class="install">
    <div class="tabs" role="tablist" aria-label="How to install">
      <button
        v-for="t in tabs"
        :key="t.id"
        role="tab"
        :aria-selected="active === t.id"
        :class="{ active: active === t.id, agent: t.id === 'agent' }"
        @click="active = t.id"
      >
        <svg v-if="t.id === 'agent'" viewBox="0 0 24 24" aria-hidden="true">
          <path d="M12 3l1.9 5.1L19 10l-5.1 1.9L12 17l-1.9-5.1L5 10l5.1-1.9L12 3Z" />
        </svg>
        {{ t.label }}
      </button>
    </div>
    <div class="line" :class="{ multi: !current.prompt }">
      <code><span v-if="current.prompt" class="prompt">{{ current.prompt }}</span>{{ current.cmd }}</code>
      <button class="copy" :aria-label="copied ? 'Copied' : 'Copy'" @click="copy">
        <svg v-if="!copied" viewBox="0 0 24 24" aria-hidden="true">
          <rect x="9" y="9" width="11" height="11" rx="2" />
          <path d="M5 15V6a2 2 0 0 1 2-2h9" />
        </svg>
        <svg v-else viewBox="0 0 24 24" aria-hidden="true"><path d="M5 12.5l4.5 4.5L19 7.5" /></svg>
      </button>
    </div>
    <p v-if="current.id === 'agent'" class="then">
      paste it into Claude Code, Codex, Cursor or any agent that can run commands.
      <a :href="withBase('/ai-agents')">What it does</a>
    </p>
    <p v-else class="then">
      then run <code>truner install</code>, or grab a binary from
      <a href="https://github.com/codician-team/truner/releases">releases</a>
    </p>
  </div>
</template>

<style scoped>
.install {
  max-width: 560px;
}

.tabs {
  display: flex;
  gap: 4px;
  margin-bottom: 8px;
}

.tabs button {
  padding: 4px 10px;
  border-radius: 6px;
  font-size: 13px;
  font-weight: 500;
  color: var(--vp-c-text-2);
  transition: color 0.2s, background-color 0.2s;
}

.tabs button:hover {
  color: var(--vp-c-text-1);
}

.tabs button.active {
  background: var(--vp-c-default-soft);
  color: var(--vp-c-text-1);
}

.tabs button.agent {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  color: var(--vp-c-brand-1);
}

.tabs button.agent svg {
  width: 13px;
  height: 13px;
  fill: currentColor;
}

/* The agent prompt is two lines of prose. */
.line.multi {
  align-items: flex-start;
}

.line.multi code {
  white-space: pre-wrap;
  font-family: var(--vp-font-family-base);
  font-size: 14px;
  line-height: 1.55;
}

.line {
  display: flex;
  align-items: center;
  border: 1px solid var(--tl-term-border);
  border-radius: 10px;
  background: var(--tl-term-bg);
}

.line code {
  flex: 1;
  min-width: 0;
  padding: 12px 0 12px 14px;
  overflow-x: auto;
  font-family: var(--vp-font-family-mono);
  font-size: 13px;
  color: var(--tl-term-text);
  white-space: nowrap;
  scrollbar-width: none;
}

.prompt {
  margin-right: 10px;
  color: var(--tl-term-dim);
  user-select: none;
}

.copy {
  flex: none;
  display: grid;
  place-items: center;
  width: 44px;
  height: 44px;
  color: var(--tl-term-dim);
  transition: color 0.2s;
}

.copy:hover {
  color: var(--tl-term-text);
}

.copy svg {
  width: 18px;
  height: 18px;
  fill: none;
  stroke: currentColor;
  stroke-width: 2;
  stroke-linecap: round;
  stroke-linejoin: round;
}

.then {
  margin: 10px 0 0;
  font-size: 13px;
  color: var(--vp-c-text-3);
}

.then code {
  font-family: var(--vp-font-family-mono);
  font-size: 12px;
  color: var(--vp-c-text-2);
}

.then a {
  color: var(--vp-c-brand-1);
  text-decoration: underline;
  text-underline-offset: 2px;
}
</style>
