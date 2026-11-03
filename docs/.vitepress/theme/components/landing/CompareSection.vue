<script setup lang="ts">
import { withBase } from 'vitepress'

type Cell = { v: string; good?: boolean; bad?: boolean }
const yes = (v = 'Yes'): Cell => ({ v, good: true })
const no = (v = 'No'): Cell => ({ v, bad: true })
const meh = (v: string): Cell => ({ v })

// truner comes first so it stays visible when the table scrolls on small screens.
const cols = ['truner', 'GitHub-hosted', 'Long-lived self-hosted']
const rows: { label: string; cells: Cell[] }[] = [
  { label: 'A clean machine for every job', cells: [yes(), yes(), no('Usually not')] },
  { label: 'Runs in your AWS account, VPC and IAM', cells: [yes(), no(), yes()] },
  { label: 'Spot pricing', cells: [yes('Automatic, with on-demand fallback'), no(), meh('Do it yourself')] },
  { label: 'Any EC2 shape: Arm64, GPUs, NVMe, 96 vCPU', cells: [yes(), meh('Fixed sizes'), yes()] },
  { label: 'Cache size', cells: [yes('No limit, in your S3'), meh('Per-repository limit'), meh('Per-repository limit')] },
  { label: 'Cost while idle', cells: [yes('≈ $0, serverless'), yes('$0'), no('Machines run 24/7')] },
  { label: 'Servers to patch and scale', cells: [yes('None'), yes('None'), no('Yours')] },
]

const costs = [
  { k: '≈ $0', v: 'while idle. Lambda, SQS, DynamoDB and API Gateway cost cents per month at low volume.' },
  { k: 'per second', v: 'of EC2 time for each job (60 s minimum), plus its disk. Spot is usually far below on-demand.' },
  { k: '$0', v: 'to truner. No license key, no per-runner fee and no SaaS in between.' },
]
</script>

<template>
  <section class="tl-section">
    <div class="tl-container">
      <span class="tl-eyebrow">Why truner</span>
      <h2 class="tl-h2">GitHub-hosted convenience at EC2 prices.</h2>
      <p class="tl-lead">
        You keep the clean-VM-per-job model of hosted runners and get the price, size and network control of running
        them yourself.
      </p>

      <div class="table-wrap">
        <table>
          <thead>
            <tr>
              <th />
              <th v-for="(c, i) in cols" :key="c" :class="{ us: i === 0 }">{{ c }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="r in rows" :key="r.label">
              <th scope="row">{{ r.label }}</th>
              <td v-for="(c, i) in r.cells" :key="i" :class="{ us: i === 0, good: c.good, bad: c.bad }">
                <span class="mark" aria-hidden="true">{{ c.good ? '✓' : c.bad ? '✕' : '–' }}</span>{{ c.v }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <div class="costs">
        <div v-for="c in costs" :key="c.k" class="cost">
          <b>{{ c.k }}</b>
          <span>{{ c.v }}</span>
        </div>
      </div>
      <p class="note">
        For a 4 vCPU Linux machine, GitHub's larger runners list at about $0.72 per hour. On spot, the same machine
        is typically $0.04–0.08 on EC2. Prices vary by region and over time: <code>truner explain</code> shows live
        spot prices and <code>truner costs</code> shows what you actually spent and saved.
        <a :href="withBase('/costs')">Costs and limits →</a>
      </p>
    </div>
  </section>
</template>

<style scoped>
.table-wrap {
  margin-top: 48px;
  overflow-x: auto;
  border: 1px solid var(--vp-c-divider);
  border-radius: 14px;
}

table {
  width: 100%;
  min-width: 640px;
  border-collapse: collapse;
  font-size: 14.5px;
}

th,
td {
  padding: 14px 18px;
  border-bottom: 1px solid var(--vp-c-divider);
  text-align: left;
  vertical-align: top;
}

tbody tr:last-child th,
tbody tr:last-child td {
  border-bottom: 0;
}

thead th {
  font-size: 13px;
  font-weight: 600;
  color: var(--vp-c-text-2);
  background: var(--vp-c-bg-soft);
}

tbody th {
  width: 32%;
  font-weight: 500;
  color: var(--vp-c-text-1);
}

td {
  color: var(--vp-c-text-2);
}

.us {
  background: var(--vp-c-brand-soft);
}

thead th.us {
  color: var(--vp-c-brand-1);
  background: var(--vp-c-brand-soft);
}

td.us {
  color: var(--vp-c-text-1);
  font-weight: 500;
}

.mark {
  display: inline-block;
  width: 20px;
  font-weight: 700;
  color: var(--vp-c-text-3);
}

.good .mark {
  color: var(--vp-c-brand-1);
}

.bad .mark {
  color: #dc2626;
}

.dark .bad .mark {
  color: #f87171;
}

.costs {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 16px;
  margin-top: 24px;
}

.cost {
  display: grid;
  gap: 6px;
  padding: 20px 22px;
  border: 1px solid var(--tl-card-border);
  border-radius: 14px;
  background: var(--tl-card-bg);
}

.cost b {
  font-size: 26px;
  font-weight: 700;
  letter-spacing: -0.02em;
  color: var(--vp-c-text-1);
}

.cost span {
  font-size: 14.5px;
  line-height: 1.55;
  color: var(--vp-c-text-2);
}

.note {
  margin: 20px 0 0;
  max-width: 860px;
  font-size: 13.5px;
  line-height: 1.6;
  color: var(--vp-c-text-3);
}

.note code {
  font-family: var(--vp-font-family-mono);
  font-size: 0.9em;
  color: var(--vp-c-text-2);
}

.note a {
  font-weight: 600;
  color: var(--vp-c-brand-1);
}

@media (max-width: 768px) {
  .costs {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
