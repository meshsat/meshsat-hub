<script setup>
import { ref, onMounted, onUnmounted, watch } from 'vue'
import { createMap, setMapTheme, probeLocalArchive, maplibregl } from '../map/basemap'
import { useThemeStore } from '../stores/theme'
import { geofences, escalation } from '../api/client'

const mapContainer = ref(null)
const fenceList = ref([])
const error = ref('')
const loading = ref(true)
const showForm = ref(false)
const formName = ref('')
const formTrigger = ref('both')
const formChainId = ref('')
// The tenant's escalation chains, so the chain a fence triggers is PICKED and
// not typed (MESHSAT-1119). A typo in a free-text field produced a fence that
// silently paged nobody -- which is the exact failure this feature was.
const chains = ref([])
const chainsLoaded = ref(false)
// Crossing cooldown (MESHSAT-1119). '' means the platform default; the policy
// endpoint supplies that number and the bounds so the form does not hardcode
// one that drifts from the ConfigMap.
const formCooldown = ref('')
const policy = ref(null)
const basemapMissing = ref(false)
const vertexCount = ref(0)
const theme = useThemeStore()

let map = null
let mapReady = false
let popup = null
let drawingPoints = []

// Map colours come from the theme tokens (read at draw time, so they follow
// the theme): an armed fence in the text colour, a disarmed one muted, and the
// fence being drawn in Signal Orange, the colour of "you are acting here".
function token(name) {
  const v = getComputedStyle(document.documentElement).getPropertyValue(`--ms-${name}`).trim()
  return v ? `rgb(${v.split(/\s+/).join(',')})` : 'rgb(128,128,128)'
}
const enabledColor = token('text')
const disabledColor = token('muted')
const drawingColor = token('primary')

function escapeHtml(v) {
  return String(v ?? '').replace(/[&<>"']/g, (c) => (
    { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
  ))
}

function addLayers() {
  map.addSource('fences', { type: 'geojson', data: { type: 'FeatureCollection', features: [] } })
  map.addSource('drawing', { type: 'geojson', data: { type: 'FeatureCollection', features: [] } })
  map.addLayer({
    id: 'fence-fill',
    type: 'fill',
    source: 'fences',
    paint: { 'fill-color': ['get', 'color'], 'fill-opacity': 0.15 },
  })
  map.addLayer({
    id: 'fence-outline',
    type: 'line',
    source: 'fences',
    paint: { 'line-color': ['get', 'color'], 'line-width': 2 },
  })
  map.addLayer({
    id: 'drawing-shape',
    type: 'fill',
    source: 'drawing',
    filter: ['==', ['geometry-type'], 'Polygon'],
    paint: { 'fill-color': drawingColor, 'fill-opacity': 0.1 },
  })
  map.addLayer({
    id: 'drawing-outline',
    type: 'line',
    source: 'drawing',
    filter: ['!=', ['geometry-type'], 'Point'],
    paint: { 'line-color': drawingColor, 'line-width': 2, 'line-dasharray': [2, 2] },
  })
  map.addLayer({
    id: 'drawing-vertices',
    type: 'circle',
    source: 'drawing',
    filter: ['==', ['geometry-type'], 'Point'],
    paint: { 'circle-radius': 5, 'circle-color': drawingColor, 'circle-stroke-color': token('bg'), 'circle-stroke-width': 1 },
  })
  map.on('click', 'fence-fill', (e) => {
    const f = e.features?.[0]
    if (!f || showForm.value) return
    popup?.remove()
    popup = new maplibregl.Popup({ maxWidth: '260px' }).setLngLat(e.lngLat).setHTML(f.properties.popup).addTo(map)
  })
  map.on('mouseenter', 'fence-fill', () => { map.getCanvas().style.cursor = 'pointer' })
  map.on('mouseleave', 'fence-fill', () => { map.getCanvas().style.cursor = '' })
  renderFences()
  updateDrawing()
}

function renderFences() {
  if (!mapReady) return
  const features = []
  for (const f of fenceList.value) {
    if (!f.polygon || f.polygon.length < 3) continue
    // Points come back as {lat, lon}; tolerate the capitalised spelling the
    // API used before the json tags landed (MESHSAT-967).
    const ring = f.polygon.map((p) => [p.lon ?? p.Lon, p.lat ?? p.Lat])
    if (ring.some(([lon, lat]) => typeof lon !== 'number' || typeof lat !== 'number')) continue
    ring.push(ring[0])
    features.push({
      type: 'Feature',
      properties: {
        color: f.enabled ? enabledColor : disabledColor,
        popup: `<b>${escapeHtml(f.name)}</b><br/>Trigger: ${escapeHtml(f.trigger)}<br/>ID: ${escapeHtml(f.id)}`,
      },
      geometry: { type: 'Polygon', coordinates: [ring] },
    })
  }
  map.getSource('fences')?.setData({ type: 'FeatureCollection', features })
}

// updateDrawing mirrors the vertices being clicked: every point as a dot, plus
// the line or the closed ring once there are enough of them.
function updateDrawing() {
  vertexCount.value = drawingPoints.length
  if (!mapReady) return
  const features = drawingPoints.map((p) => ({
    type: 'Feature', properties: {}, geometry: { type: 'Point', coordinates: [p.lon, p.lat] },
  }))
  const coords = drawingPoints.map((p) => [p.lon, p.lat])
  if (coords.length === 2) {
    features.push({ type: 'Feature', properties: {}, geometry: { type: 'LineString', coordinates: coords } })
  } else if (coords.length >= 3) {
    features.push({
      type: 'Feature', properties: {},
      geometry: { type: 'Polygon', coordinates: [[...coords, coords[0]]] },
    })
  }
  map.getSource('drawing')?.setData({ type: 'FeatureCollection', features })
}

function onMapClick(e) {
  if (!showForm.value) return
  drawingPoints.push({ lat: e.lngLat.lat, lon: e.lngLat.lng })
  updateDrawing()
}

function clearDrawing() {
  drawingPoints = []
  updateDrawing()
}

async function loadFences() {
  loading.value = true
  try {
    fenceList.value = await geofences.list() || []
    renderFences()
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

// Both were called from onMounted since MESHSAT-1119 but never written, so
// the page threw a ReferenceError on every load: the chain picker never got
// its chains and the cooldown bounds never arrived. A failure here leaves the
// form usable (typed chain id, platform default cooldown), never blocks it.
async function loadChains() {
  try {
    const c = await escalation.listChains()
    chains.value = Array.isArray(c) ? c : []
    chainsLoaded.value = true
  } catch {
    chainsLoaded.value = false
  }
}

async function loadPolicy() {
  try {
    policy.value = await geofences.policy()
  } catch {
    policy.value = null
  }
}

async function saveFence() {
  if (drawingPoints.length < 3) {
    error.value = 'Click at least 3 points on the map to define a polygon'
    return
  }
  if (!formName.value) {
    error.value = 'Name is required'
    return
  }
  error.value = ''
  try {
    await geofences.create({
      name: formName.value,
      polygon: drawingPoints,
      trigger: formTrigger.value,
      chain_id: formChainId.value || undefined,
      cooldown_sec: formCooldown.value.trim() === '' ? 0 : Number(formCooldown.value),
      enabled: true,
    })
    formName.value = ''
    formTrigger.value = 'both'
    formChainId.value = ''
    formCooldown.value = ''
    showForm.value = false
    clearDrawing()
    await loadFences()
  } catch (e) {
    error.value = e.message
  }
}

async function removeFence(id) {
  if (!confirm('Delete this geofence?')) return
  try {
    await geofences.delete(id)
    await loadFences()
  } catch (e) {
    error.value = e.message
  }
}

function startDrawing() {
  showForm.value = true
  clearDrawing()
}

function cancelDrawing() {
  showForm.value = false
  clearDrawing()
}

onMounted(async () => {
  // Resolve the deeper regional archive first: the style is built once.
  await probeLocalArchive()
  map = createMap({
    container: mapContainer.value,
    center: [4.9, 52.37],
    zoom: 3,
    dark: theme.dark,
    onBasemapError: () => { basemapMissing.value = true },
  })
  map.on('load', () => {
    mapReady = true
    addLayers()
  })
  map.on('click', onMapClick)
  await loadFences()
  await loadChains()
  await loadPolicy()
})

watch(() => theme.dark, async (dark) => {
  if (!map) return
  mapReady = false
  await setMapTheme(map, dark)
  mapReady = true
  addLayers()
})

onUnmounted(() => {
  popup?.remove()
  if (map) map.remove()
})
</script>

<template>
  <div class="ms-page">
    <div class="ms-page-head">
      <div>
        <h1 class="ms-h1">Geofences</h1>
        <p class="ms-lede">Areas on the map. A device that crosses into or out of one raises an alert.</p>
      </div>
      <button v-if="!showForm" @click="startDrawing"
        class="ms-btn-primary">
        + Draw fence
      </button>
    </div>

    <div v-if="error" role="alert" class="ms-alert mb-4">{{ error }}</div>

    <div v-if="basemapMissing"
      class="mb-4 rounded border border-amber-700/50 bg-amber-900/20 text-amber-200 text-xs px-3 py-2">
      The self-hosted basemap is unavailable, so fences are drawn on an empty backdrop. Drawing and editing still work.
    </div>

    <!-- Drawing form -->
    <div v-if="showForm" class="bg-tactical-surface rounded-lg border border-amber-700 p-4 mb-4">
      <h2 class="text-sm font-semibold text-ms-warning mb-2">Drawing mode: click the map to add vertices</h2>
      <div class="grid grid-cols-1 sm:grid-cols-3 gap-3 mb-3">
        <input v-model="formName" placeholder="Fence name" class="bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm">
        <select v-model="formTrigger" class="bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm">
          <option value="enter">Enter</option>
          <option value="exit">Exit</option>
          <option value="both">Enter + Exit</option>
        </select>
        <!-- Picked, not typed (MESHSAT-1119): a typo in a free-text chain id
             produced a fence that silently paged nobody, which is exactly the
             failure this feature was. Falls back to the text field if the
             chain list cannot be loaded, rather than blocking the form. -->
        <select v-if="chainsLoaded" v-model="formChainId"
          class="bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm">
          <option value="">No alert, record crossings only</option>
          <option v-for="c in chains" :key="c.id" :value="c.id">{{ c.name || c.id }}</option>
        </select>
        <input v-else v-model="formChainId" placeholder="Escalation chain ID (optional)"
          class="bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm">
        <!-- Crossing cooldown (MESHSAT-1119). A fence fires the moment the
             device crosses the line, so one parked on a boundary would page
             every few minutes; this is how long it then stays quiet for that
             device. The FIRST crossing is never delayed. -->
        <input v-model="formCooldown" type="number" inputmode="numeric"
          :min="policy?.cooldown_min" :max="policy?.cooldown_max"
          :placeholder="policy ? `Quiet for ${policy.cooldown_default}s after alerting` : 'Cooldown (seconds)'"
          class="bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm">
      </div>
      <p v-if="policy" class="text-xs text-gray-400 -mt-1">
        After a crossing alerts, this fence stays quiet for that device for the cooldown
        ({{ policy.cooldown_default }}s by default, {{ policy.cooldown_min }}&ndash;{{ policy.cooldown_max }}s).
        The first crossing is never delayed. This only stops a device sitting on the
        boundary from alerting repeatedly.
      </p>
      <div class="flex gap-2">
        <span class="text-xs text-gray-400">{{ vertexCount }} vertices</span>
        <button @click="clearDrawing" class="text-xs text-gray-400 hover:text-gray-200">Clear</button>
        <div class="flex-1"></div>
        <button @click="cancelDrawing" class="text-gray-400 hover:text-gray-300 text-sm px-3 py-1">Cancel</button>
        <button @click="saveFence" class="ms-btn-primary">Save</button>
      </div>
    </div>

    <!-- Map -->
    <div ref="mapContainer" class="w-full rounded-lg overflow-hidden mb-4 bg-ms-bg2" style="height: calc(100vh - 300px); min-height: 400px;"></div>

    <!-- Fence list -->
    <div v-if="fenceList.length > 0" class="bg-tactical-surface rounded-lg border border-tactical-border overflow-hidden">
      <div class="px-4 py-3 border-b border-tactical-border">
        <h2 class="text-sm font-sans font-semibold text-gray-200">Configured Fences ({{ fenceList.length }})</h2>
      </div>
      <div class="divide-y divide-tactical-border/50">
        <div v-for="f in fenceList" :key="f.id" class="px-4 py-3 flex items-center justify-between">
          <div>
            <span class="text-gray-300 text-sm font-medium">{{ f.name }}</span>
            <span class="text-gray-500 text-xs ml-2">{{ f.trigger }}</span>
            <span class="text-ms-muted text-xs ml-2 font-mono">{{ f.polygon?.length }} pts</span>
          </div>
          <button @click="removeFence(f.id)" class="text-ms-error hover:text-red-300 text-xs">Delete</button>
        </div>
      </div>
    </div>
  </div>
</template>
