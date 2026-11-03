<script setup lang="ts">
import CompareSection from './landing/CompareSection.vue'
import CtaFooter from './landing/CtaFooter.vue'
import FaqSection from './landing/FaqSection.vue'
import FeatureGrid from './landing/FeatureGrid.vue'
import HeroSection from './landing/HeroSection.vue'
import HowItWorks from './landing/HowItWorks.vue'
import LabelSection from './landing/LabelSection.vue'
import OpsSection from './landing/OpsSection.vue'
import ResilienceSection from './landing/ResilienceSection.vue'
</script>

<template>
  <div class="landing">
    <HeroSection />
    <LabelSection />
    <FeatureGrid />
    <HowItWorks />
    <ResilienceSection />
    <CompareSection />
    <OpsSection />
    <FaqSection />
    <CtaFooter />
  </div>
</template>

<style scoped>
.landing {
  overflow-x: clip;
}

/* The label section directly follows the hero, which has no border of its own. */
.landing > :nth-child(2) {
  border-top: 1px solid var(--vp-c-divider);
}
</style>
