<script setup>
import { ref, onMounted, onUnmounted, watch } from 'vue'
import { createMap, setMapTheme, probeLocalArchive, maplibregl } from '../map/basemap'
import { useThemeStore } from '../stores/theme'
import { geofences } from '../api/client'

const mapContainer = ref(null)
const fenceList = ref([])
const error = ref('')
const loading = ref(true)
const showForm = ref(false)
const formName = ref('')
const formTrigger = ref('both')
const formChainId = ref('')
const basemapMissing = ref(false)
const vertexCount = ref(0)
const theme = useThemeStore()

let map = null
let mapReady = false
let popup = null
let drawingPoints = []

const enabledColor = '#2dd4bf'
const disabledColor = '#6b7280'
const drawingColor = '#f59e0b'

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
    paint: { 'circle-radius': 5, 'circle-color': drawingColor, 'circle-stroke-color': '#fff', 'circle-stroke-width': 1 },
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
      enabled: true,
    })
    formName.value = ''
    formTrigger.value = 'both'
    formChainId.value = ''
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
  <div class="p-4 lg:p-6">
    <div class="flex items-center justify-between mb-4">
      <h1 class="text-2xl font-display font-bold">Geofences</h1>
      <button v-if="!showForm" @click="startDrawing"
        class="bg-brand-accent hover:bg-brand-primary text-ms-on-primary text-sm px-4 py-2 rounded">
        + Draw Fence
      </button>
    </div>

    <div v-if="error" class="bg-red-900/50 border border-red-700 text-red-200 rounded p-3 mb-4">{{ error }}</div>

    <div v-if="basemapMissing"
      class="mb-4 rounded border border-amber-700/50 bg-amber-900/20 text-amber-200 text-xs px-3 py-2">
      The self-hosted basemap is unavailable, so fences are drawn on an empty backdrop. Drawing and editing still work.
    </div>

    <!-- Drawing form -->
    <div v-if="showForm" class="bg-tactical-surface rounded-lg border border-amber-700 p-4 mb-4">
      <h2 class="text-sm font-semibold text-ms-warning uppercase tracking-wider mb-2">Drawing mode — click map to add vertices</h2>
      <div class="grid grid-cols-1 sm:grid-cols-3 gap-3 mb-3">
        <input v-model="formName" placeholder="Fence name" class="bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm">
        <select v-model="formTrigger" class="bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm">
          <option value="enter">Enter</option>
          <option value="exit">Exit</option>
          <option value="both">Enter + Exit</option>
        </select>
        <input v-model="formChainId" placeholder="Escalation chain ID (optional)" class="bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm">
      </div>
      <div class="flex gap-2">
        <span class="text-xs text-gray-400">{{ vertexCount }} vertices</span>
        <button @click="clearDrawing" class="text-xs text-gray-400 hover:text-gray-200">Clear</button>
        <div class="flex-1"></div>
        <button @click="cancelDrawing" class="text-gray-400 hover:text-gray-300 text-sm px-3 py-1">Cancel</button>
        <button @click="saveFence" class="bg-brand-accent hover:bg-brand-primary text-ms-on-primary text-sm px-4 py-1 rounded">Save</button>
      </div>
    </div>

    <!-- Map -->
    <div ref="mapContainer" class="w-full rounded-lg overflow-hidden mb-4 bg-ms-bg2" style="height: calc(100vh - 300px); min-height: 400px;"></div>

    <!-- Fence list -->
    <div v-if="fenceList.length > 0" class="bg-tactical-surface rounded-lg border border-tactical-border overflow-hidden">
      <div class="px-4 py-3 border-b border-tactical-border">
        <h2 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider">Configured Fences ({{ fenceList.length }})</h2>
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
