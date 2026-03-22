<script setup>
import { ref, onMounted, onUnmounted } from 'vue'
import L from 'leaflet'
import { geofences } from '../api/client'

const mapContainer = ref(null)
const fenceList = ref([])
const error = ref('')
const loading = ref(true)
const showForm = ref(false)
const formName = ref('')
const formTrigger = ref('both')
const formChainId = ref('')
let map = null
let drawnPolygons = {}
let drawingPoints = []
let drawingMarkers = []
let drawingPoly = null

onMounted(async () => {
  map = L.map(mapContainer.value).setView([52.37, 4.90], 4)
  L.tileLayer('https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png', {
    attribution: '&copy; OpenStreetMap &copy; CARTO',
    maxZoom: 19,
  }).addTo(map)

  map.on('click', onMapClick)
  await loadFences()
})

onUnmounted(() => {
  if (map) map.remove()
})

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

function renderFences() {
  Object.values(drawnPolygons).forEach(p => map.removeLayer(p))
  drawnPolygons = {}
  for (const f of fenceList.value) {
    if (!f.polygon || f.polygon.length < 3) continue
    const latlngs = f.polygon.map(p => [p.lat, p.lon])
    const poly = L.polygon(latlngs, {
      color: f.enabled ? '#2dd4bf' : '#6b7280',
      fillOpacity: 0.15,
      weight: 2,
    }).addTo(map).bindPopup(`<b>${f.name}</b><br/>Trigger: ${f.trigger}<br/>ID: ${f.id}`)
    drawnPolygons[f.id] = poly
  }
}

function onMapClick(e) {
  if (!showForm.value) return
  drawingPoints.push({ lat: e.latlng.lat, lon: e.latlng.lng })
  const marker = L.circleMarker(e.latlng, { radius: 5, color: '#f59e0b', fillOpacity: 1 }).addTo(map)
  drawingMarkers.push(marker)
  updateDrawingPoly()
}

function updateDrawingPoly() {
  if (drawingPoly) map.removeLayer(drawingPoly)
  if (drawingPoints.length >= 2) {
    const latlngs = drawingPoints.map(p => [p.lat, p.lon])
    drawingPoly = L.polygon(latlngs, { color: '#f59e0b', fillOpacity: 0.1, dashArray: '5,5' }).addTo(map)
  }
}

function clearDrawing() {
  drawingPoints = []
  drawingMarkers.forEach(m => map.removeLayer(m))
  drawingMarkers = []
  if (drawingPoly) { map.removeLayer(drawingPoly); drawingPoly = null }
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
</script>

<template>
  <div class="p-4 lg:p-6">
    <div class="flex items-center justify-between mb-4">
      <h1 class="text-2xl font-display font-bold">Geofences</h1>
      <button v-if="!showForm" @click="startDrawing"
        class="bg-teal-600 hover:bg-teal-500 text-white text-sm px-4 py-2 rounded">
        + Draw Fence
      </button>
    </div>

    <div v-if="error" class="bg-red-900/50 border border-red-700 text-red-200 rounded p-3 mb-4">{{ error }}</div>

    <!-- Drawing form -->
    <div v-if="showForm" class="bg-tactical-surface rounded-lg border border-amber-700 p-4 mb-4">
      <h2 class="text-sm font-semibold text-amber-400 uppercase tracking-wider mb-2">Drawing mode — click map to add vertices</h2>
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
        <span class="text-xs text-gray-400">{{ drawingPoints.length }} vertices</span>
        <button @click="clearDrawing" class="text-xs text-gray-400 hover:text-gray-200">Clear</button>
        <div class="flex-1"></div>
        <button @click="cancelDrawing" class="text-gray-400 hover:text-gray-300 text-sm px-3 py-1">Cancel</button>
        <button @click="saveFence" class="bg-teal-600 hover:bg-teal-500 text-white text-sm px-4 py-1 rounded">Save</button>
      </div>
    </div>

    <!-- Map -->
    <div ref="mapContainer" class="w-full rounded-lg overflow-hidden mb-4" style="height: calc(100vh - 300px); min-height: 400px;"></div>

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
            <span class="text-gray-600 text-xs ml-2 font-mono">{{ f.polygon?.length }} pts</span>
          </div>
          <button @click="removeFence(f.id)" class="text-red-400 hover:text-red-300 text-xs">Delete</button>
        </div>
      </div>
    </div>
  </div>
</template>
