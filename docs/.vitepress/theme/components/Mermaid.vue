<script setup lang="ts">
import { useData } from 'vitepress'
import { onMounted, ref, watch } from 'vue'

const props = defineProps<{ code: string }>()
const { isDark } = useData()
const svg = ref('')
let seq = 0

async function render() {
  const { default: mermaid } = await import('mermaid')
  mermaid.initialize({
    startOnLoad: false,
    securityLevel: 'strict',
    theme: isDark.value ? 'dark' : 'neutral',
    fontFamily: 'var(--vp-font-family-base)',
  })
  const id = `mermaid-${Math.random().toString(36).slice(2)}-${seq++}`
  svg.value = (await mermaid.render(id, decodeURIComponent(props.code))).svg
}

onMounted(render)
watch(isDark, render)
</script>

<template>
  <div class="mermaid-diagram" :class="{ loading: !svg }" v-html="svg" />
</template>

<style scoped>
.mermaid-diagram {
  margin: 16px 0;
  overflow-x: auto;
  text-align: center;
}
.mermaid-diagram.loading {
  min-height: 240px;
  border-radius: 8px;
  background: var(--vp-c-bg-soft);
}
.mermaid-diagram :deep(svg) {
  max-width: 100%;
  height: auto;
}
</style>
