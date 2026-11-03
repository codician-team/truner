import { useData } from 'vitepress'
import { onMounted, ref } from 'vue'

export const repoSlug = 'codician-team/truner'
const cacheKey = 'truner-stars'
const maxAge = 60 * 60 * 1000

// The repository's star count. The page renders with the count fetched when
// the site was built (themeConfig.stars) and refreshes it from the GitHub API
// at most once an hour per visitor, which stays within the API's anonymous
// rate limit.
export function useStars() {
  const { theme } = useData()
  const stars = ref<number | null>(theme.value.stars ?? null)
  onMounted(async () => {
    try {
      const cached = JSON.parse(sessionStorage.getItem(cacheKey) ?? 'null')
      if (cached && Date.now() - cached.at < maxAge) {
        stars.value = cached.n
        return
      }
    } catch {
      // Storage can be unavailable (private windows, blocked cookies).
    }
    try {
      const res = await fetch(`https://api.github.com/repos/${repoSlug}`)
      const n = res.ok ? (await res.json()).stargazers_count : undefined
      if (typeof n === 'number') {
        stars.value = n
        sessionStorage.setItem(cacheKey, JSON.stringify({ n, at: Date.now() }))
      }
    } catch {
      // Offline or rate limited: keep the build-time count.
    }
  })
  return stars
}

// 950 → "950", 1234 → "1.2k", 15300 → "15k"
export function formatStars(n: number): string {
  if (n < 1000) return String(n)
  return (n / 1000).toFixed(n < 10000 ? 1 : 0).replace(/\.0$/, '') + 'k'
}
