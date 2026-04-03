import { defineStore } from 'pinia'
import { ref, watch } from 'vue'

const STORAGE_KEY = 'meshsat-dashboard-layout'

const defaultWidgets = [
  { id: 'kpi', label: 'KPI Cards', visible: true },
  { id: 'constellations', label: 'Constellations', visible: true },
  { id: 'safety', label: 'Safety Status', visible: true },
  { id: 'network', label: 'Network', visible: true },
  { id: 'activity', label: 'Message Activity', visible: true },
  { id: 'messages', label: 'Recent Messages', visible: true },
  { id: 'fleet', label: 'Device Fleet', visible: true },
  { id: 'budgets', label: 'Budget Usage', visible: true },
]

export const useDashboardStore = defineStore('dashboard', () => {
  const widgets = ref(loadWidgets())
  const customizing = ref(false)

  function loadWidgets() {
    try {
      const saved = localStorage.getItem(STORAGE_KEY)
      if (saved) {
        const parsed = JSON.parse(saved)
        // Merge with defaults to pick up any new widgets
        const known = new Set(parsed.map(w => w.id))
        const merged = [...parsed]
        for (const d of defaultWidgets) {
          if (!known.has(d.id)) merged.push({ ...d })
        }
        return merged
      }
    } catch { /* ignore */ }
    return defaultWidgets.map(w => ({ ...w }))
  }

  function save() {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(widgets.value))
  }

  function toggle(id) {
    const w = widgets.value.find(w => w.id === id)
    if (w) { w.visible = !w.visible; save() }
  }

  function moveUp(id) {
    const idx = widgets.value.findIndex(w => w.id === id)
    if (idx > 0) {
      const tmp = widgets.value[idx]
      widgets.value[idx] = widgets.value[idx - 1]
      widgets.value[idx - 1] = tmp
      save()
    }
  }

  function moveDown(id) {
    const idx = widgets.value.findIndex(w => w.id === id)
    if (idx >= 0 && idx < widgets.value.length - 1) {
      const tmp = widgets.value[idx]
      widgets.value[idx] = widgets.value[idx + 1]
      widgets.value[idx + 1] = tmp
      save()
    }
  }

  function reset() {
    widgets.value = defaultWidgets.map(w => ({ ...w }))
    save()
  }

  function isVisible(id) {
    const w = widgets.value.find(w => w.id === id)
    return w ? w.visible : true
  }

  function orderedVisible() {
    return widgets.value.filter(w => w.visible).map(w => w.id)
  }

  // TAK status (populated by Dashboard.vue from integrations API)
  const takEnabled = ref(false)
  const takFedPeers = ref(0)
  const takFedIn = ref(0)
  const takFedOut = ref(0)
  const takMissions = ref(0)

  function setTakStatus(enabled, fedPeers, fedIn, fedOut, missions) {
    takEnabled.value = enabled
    takFedPeers.value = fedPeers || 0
    takFedIn.value = fedIn || 0
    takFedOut.value = fedOut || 0
    takMissions.value = missions || 0
  }

  return { widgets, customizing, toggle, moveUp, moveDown, reset, isVisible, orderedVisible, takEnabled, takFedPeers, takFedIn, takFedOut, takMissions, setTakStatus }
})
