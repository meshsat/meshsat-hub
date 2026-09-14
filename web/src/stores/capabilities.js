import { defineStore } from 'pinia'
import { ref } from 'vue'
import { capabilities as api } from '../api/client'

// Which features will actually do something for this tenant (MESHSAT-1121).
//
// Before this, a page worked out its own availability by matching an error
// message against /not found|404/i. That missed a 403 -- so a viewer without
// permission was shown an empty table and told there was no data, when the truth
// was they were not allowed to look. `GET /api/capabilities` answers the
// question directly, per tenant.
export const useCapabilitiesStore = defineStore('capabilities', () => {
  const byFeature = ref({})
  const loaded = ref(false)
  const loading = ref(false)

  async function load(force = false) {
    if (loading.value || (loaded.value && !force)) return
    loading.value = true
    try {
      const list = await api.list()
      const next = {}
      for (const c of list || []) next[c.feature] = c
      byFeature.value = next
      loaded.value = true
    } catch {
      // Deliberately swallowed, and deliberately NOT marked loaded.
      //
      // A failed fetch must not render every feature as unavailable: that would
      // tell a customer their working notifications are switched off because one
      // request lost a race with a rollout. Unknown reads as available below,
      // and the page falls back to whatever the feature's own API says.
      loaded.value = false
    } finally {
      loading.value = false
    }
  }

  function get(feature) {
    return byFeature.value[feature] || null
  }

  // Unknown means available. See the catch above: the honest failure mode is to
  // let the feature's own endpoint speak, not to declare it dead.
  function isConfigured(feature) {
    const c = get(feature)
    return c ? c.configured : true
  }

  function reason(feature) {
    const c = get(feature)
    return c && !c.configured ? c.reason : ''
  }

  // True only when we KNOW it is unconfigured, which is what a page should gate
  // its "not set up" state on.
  function isUnavailable(feature) {
    const c = get(feature)
    return !!c && !c.configured
  }

  return { byFeature, loaded, loading, load, get, isConfigured, isUnavailable, reason }
})
