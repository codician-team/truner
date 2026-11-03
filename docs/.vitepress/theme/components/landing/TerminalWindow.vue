<script setup lang="ts">
defineProps<{ title?: string }>()
</script>

<template>
  <div class="term">
    <div class="term-bar">
      <span class="dot" /><span class="dot" /><span class="dot" />
      <span v-if="title" class="term-title">{{ title }}</span>
    </div>
    <div class="term-body"><slot /></div>
  </div>
</template>

<style scoped>
.term {
  overflow: hidden;
  border: 1px solid var(--tl-term-border);
  border-radius: 12px;
  background: var(--tl-term-bg);
  box-shadow:
    0 1px 2px rgba(0, 0, 0, 0.12),
    0 24px 48px -16px rgba(2, 6, 23, 0.45);
  color: var(--tl-term-text);
  text-align: left;
}

.term-bar {
  display: flex;
  align-items: center;
  gap: 7px;
  height: 36px;
  padding: 0 14px;
  border-bottom: 1px solid var(--tl-term-border);
  background: var(--tl-term-bar);
}

.dot {
  width: 11px;
  height: 11px;
  border-radius: 50%;
  background: #334155;
}

.term-title {
  margin-left: 8px;
  font-family: var(--vp-font-family-mono);
  font-size: 12px;
  color: var(--tl-term-dim);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.term-body {
  padding: 16px 18px 18px;
  overflow-x: auto;
  font-family: var(--vp-font-family-mono);
  font-size: 13px;
  line-height: 1.65;
}

@media (max-width: 640px) {
  .term-body {
    padding: 14px;
    font-size: 12px;
  }
}
</style>
