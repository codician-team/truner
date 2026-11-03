<script setup lang="ts">
import { withBase } from 'vitepress'

const faqs = [
  {
    q: 'What do I need to get started?',
    a: 'An AWS account where you can create resources (administrator access for the install) and a GitHub organization or personal account. Then run `truner install`, which asks for AWS keys if none are configured, deploys the stack and walks you through creating the GitHub App in your browser.',
  },
  {
    q: 'How fast does a job start?',
    a: 'Most of the time goes to EC2 booting the machine. Baked images skip software installation at boot, and warm pools skip the cold boot entirely. The first job on a new image is slower while its optimized image is being built. Every job log shows the boot timings.',
  },
  {
    q: 'What happens on a spot interruption?',
    a: 'If it happens before the job starts, truner launches an on-demand machine instead. If it happens during the job, GitHub marks the job as failed, and truner re-runs it on on-demand capacity once the workflow run finishes. You can turn that off with `retryOnInterruption: false`.',
  },
  {
    q: 'Do I have to change my workflows to use the cache?',
    a: 'No. `actions/cache`, `setup-node`, `setup-python`, `setup-go`, `setup-java`, Gradle and `docker buildx` with `type=gha` talk to a cache proxy on the machine, which stores the cache in your S3 bucket. Older actions that use the legacy cache protocol work too.',
  },
  {
    q: 'Which GitHub plans work?',
    a: 'Any github.com plan (Free, Team or Enterprise Cloud), for organizations and personal accounts. Check GitHub\'s current pricing page for how self-hosted runner usage is billed on your plan. GitHub Enterprise Server is not supported yet.',
  },
  {
    q: 'Are macOS runners supported?',
    a: 'No. EC2 Mac instances require dedicated hosts with a 24-hour minimum allocation, which does not fit a machine-per-job model.',
  },
  {
    q: 'Is it safe for public repositories?',
    a: 'Each job runs on a fresh VM with a single-use registration that only works for its own repository. All repositories in one installation share the same cache bucket and machine role, though. Use separate installations for different trust levels, and do not run pull requests from forks on self-hosted runners.',
  },
  {
    q: 'How do I remove it?',
    a: '`truner uninstall` terminates every machine and deletes the baked images, the bucket and the stack. After that, delete the GitHub App in your GitHub settings, because GitHub has no API for removing apps.',
  },
]

function md(s: string) {
  return s.replace(/[&<>]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;' })[c]!).replace(/`([^`]+)`/g, '<code>$1</code>')
}
</script>

<template>
  <section class="tl-section">
    <div class="tl-container grid">
      <div>
        <span class="tl-eyebrow">FAQ</span>
        <h2 class="tl-h2">Questions, answered.</h2>
        <p class="tl-lead">
          Something missing? Read the <a :href="withBase('/faq')">full FAQ</a> or
          <a href="https://github.com/truner/truner/issues">open an issue</a>.
        </p>
      </div>
      <div class="list">
        <details v-for="f in faqs" :key="f.q">
          <summary>{{ f.q }}</summary>
          <p v-html="md(f.a)" />
        </details>
      </div>
    </div>
  </section>
</template>

<style scoped>
.grid {
  display: grid;
  grid-template-columns: minmax(0, 0.8fr) minmax(0, 1.2fr);
  gap: 48px;
}

.tl-lead a {
  font-weight: 500;
  color: var(--vp-c-brand-1);
  text-decoration: underline;
  text-underline-offset: 3px;
}

details {
  border-bottom: 1px solid var(--vp-c-divider);
}

details:first-child {
  border-top: 1px solid var(--vp-c-divider);
}

summary {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  padding: 18px 0;
  font-size: 16px;
  font-weight: 600;
  color: var(--vp-c-text-1);
  cursor: pointer;
  list-style: none;
}

summary::-webkit-details-marker {
  display: none;
}

summary::after {
  content: '+';
  flex: none;
  font-size: 22px;
  font-weight: 400;
  line-height: 1;
  color: var(--vp-c-text-3);
  transition: transform 0.2s;
}

details[open] summary::after {
  transform: rotate(45deg);
}

summary:hover {
  color: var(--vp-c-brand-1);
}

details p {
  margin: -4px 0 20px;
  font-size: 15px;
  line-height: 1.65;
  color: var(--vp-c-text-2);
}

details p :deep(code) {
  font-family: var(--vp-font-family-mono);
  font-size: 0.86em;
  padding: 1px 5px;
  border-radius: 4px;
  background: var(--vp-c-default-soft);
  color: var(--vp-c-text-1);
}

@media (max-width: 960px) {
  .grid {
    grid-template-columns: minmax(0, 1fr);
    gap: 24px;
  }
}
</style>
