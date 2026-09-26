import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import { admin as adminApi } from '../api/client'
import { useAuthStore } from './auth'

// The platform's own counter: account requests waiting for a decision. One
// poll for the rail badge and the Requests page, every 90 s, and only while
// the tab is visible and the viewer is a platform admin. Nothing here is
// urgent in the ISA-18.2 sense; it is work waiting, not an alarm.
const POLL_MS = 90000

export const usePlatformStore = defineStore('platform', () => {
  const auth = useAuthStore()
  const pendingSignups = ref(0)
  const loaded = ref(false)
  // 502/503 means the Hub has no identity provider to ask. The page says so;
  // the poll stops rather than asking a question with a known answer.
  const unavailable = ref(false)
  const unavailableStatus = ref(0)

  let users = 0
  let timer = null
  let inflight = null

  const active = computed(() => auth.isPlatformAdmin)

  async function load() {
    if (!active.value) return
    if (typeof document !== 'undefined' && document.visibilityState !== 'visible') return
    if (inflight) return inflight
    inflight = (async () => {
      try {
        const res = await adminApi.signups.list()
        const list = Array.isArray(res?.signups) ? res.signups : []
        // Probe accounts are the nightly job's, not work for a person.
        pendingSignups.value = list.filter((s) => !s.probe).length
        unavailable.value = false
        unavailableStatus.value = 0
        loaded.value = true
      } catch (e) {
        if (e?.status === 502 || e?.status === 503) {
          unavailable.value = true
          unavailableStatus.value = e.status
          stopTimer()
        }
      }
    })()
    try { await inflight } finally { inflight = null }
  }

  function startTimer() {
    if (timer) return
    timer = setInterval(load, POLL_MS)
  }
  function stopTimer() {
    clearInterval(timer)
    timer = null
  }
  function onVisibility() {
    if (document.visibilityState === 'visible' && users > 0 && !unavailable.value) load()
  }

  // Ref-counted like ops.js: the shell holds one subscription while a
  // platform admin is signed in; pages may hold more without a second timer.
  function start() {
    users++
    if (users > 1) return
    load()
    startTimer()
    document.addEventListener('visibilitychange', onVisibility)
  }

  function stop() {
    users = Math.max(0, users - 1)
    if (users > 0) return
    stopTimer()
    document.removeEventListener('visibilitychange', onVisibility)
  }

  // After a decision the count is known to be wrong; ask again now, and if
  // the provider had been away, give it another chance.
  function nudge() {
    unavailable.value = false
    if (users > 0) startTimer()
    return load()
  }

  return { pendingSignups, loaded, unavailable, unavailableStatus, load, start, stop, nudge }
})
