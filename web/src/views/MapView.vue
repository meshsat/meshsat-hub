<script setup>
import { ref, onMounted, onUnmounted } from 'vue'
import L from 'leaflet'
import { positions } from '../api/client'

const mapContainer = ref(null)
let map = null
let markers = {}
let refreshInterval = null

const trackData = ref({})
const trackRange = ref('24h')
let activeTrackImei = null
let trackLayer = null

const rangeOptions = [
  { label: '24h', value: '24h' },
  { label: '7d', value: '7d' },
  { label: '30d', value: '30d' },
]

function rangeToISO(range) {
  const now = new Date()
  const ms = { '24h': 24 * 3600000, '7d': 7 * 86400000, '30d': 30 * 86400000 }
  return new Date(now.getTime() - (ms[range] || ms['24h'])).toISOString()
}

function clearTrack() {
  if (trackLayer && map) {
    map.removeLayer(trackLayer)
    trackLayer = null
  }
}

async function loadTrack(imei) {
  clearTrack()
  const from = rangeToISO(trackRange.value)
  const to = new Date().toISOString()
  try {
    const pts = await positions.historyRange(imei, from, to)
    if (!pts || pts.length === 0) {
      activeTrackImei = imei
      return
    }
    const latlngs = pts
      .filter(p => p.lat !== 0 || p.lon !== 0)
      .map(p => [p.lat, p.lon])
    if (latlngs.length < 2) {
      activeTrackImei = imei
      return
    }
    const markerColor = markers[imei]?.options?.fillColor || '#22d3ee'
    trackLayer = L.polyline(latlngs, {
      color: markerColor,
      weight: 2,
      opacity: 0.6,
    }).addTo(map)
    activeTrackImei = imei
  } catch (e) {
    console.error('Failed to load track:', e)
  }
}

function onMarkerClick(imei) {
  if (activeTrackImei === imei) {
    clearTrack()
    activeTrackImei = null
  } else {
    loadTrack(imei)
  }
}

// TAK/CoT marker icon system
function cotColor(source) {
  if (source === 'iridium_cep' || source === 'iridium') return '#4A90D9'
  if (source === 'bridge') return '#4A90D9'
  if (source === 'emergency' || source === 'sos') return '#FF0000'
  if (source === 'astrocast') return '#9B59B6'
  return '#4A90D9'
}

function cotMarkerIcon(source, label, stale) {
  const color = cotColor(source)
  const opacity = stale ? 0.45 : 1.0
  const shortLabel = label && label.length > 8 ? label.slice(-6) : (label || '')
  const labelSvg = shortLabel ? `<text x="16" y="39" text-anchor="middle" fill="#fff" font-size="9" font-family="sans-serif" style="text-shadow:0 0 3px #000">${shortLabel}</text>` : ''

  // Bridges → square, devices → diamond, Iridium → diamond with amber accent
  let shape
  if (source === 'bridge') {
    shape = `<rect x="3" y="3" width="26" height="26" fill="${color}" stroke="#fff" stroke-width="2" rx="2"/>`
  } else if (source === 'emergency' || source === 'sos') {
    shape = `<circle cx="16" cy="16" r="14" fill="#FF0000" stroke="#fff" stroke-width="2"/><line x1="8" y1="8" x2="24" y2="24" stroke="#fff" stroke-width="3"/><line x1="24" y1="8" x2="8" y2="24" stroke="#fff" stroke-width="3"/>`
  } else {
    shape = `<polygon points="16,2 30,16 16,30 2,16" fill="${color}" stroke="#fff" stroke-width="2"/>`
  }

  const svg = `<svg width="32" height="42" viewBox="0 0 32 42" opacity="${opacity}">${shape}${labelSvg}</svg>`
  return L.divIcon({ html: svg, className: '', iconSize: [32, 42], iconAnchor: [16, 30], popupAnchor: [0, -30] })
}

function onRangeChange() {
  if (activeTrackImei) {
    loadTrack(activeTrackImei)
  }
}

onMounted(async () => {
  map = L.map(mapContainer.value).setView([52.37, 4.90], 4)

  L.tileLayer('https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png', {
    attribution: '&copy; OpenStreetMap &copy; CARTO',
    maxZoom: 19,
  }).addTo(map)

  await refreshPositions()
  refreshInterval = setInterval(refreshPositions, 30000)
})

onUnmounted(() => {
  if (refreshInterval) clearInterval(refreshInterval)
  if (map) map.remove()
})

async function refreshPositions() {
  try {
    const data = await positions.allLatest()
    for (const pos of data) {
      if (pos.lat === 0 && pos.lon === 0) continue

      const key = pos.imei
      const label = pos.label || pos.imei
      const popup = `<b>${label}</b><br/>
        IMEI: ${pos.imei}<br/>
        ${pos.lat.toFixed(6)}, ${pos.lon.toFixed(6)}<br/>
        Source: ${pos.source}<br/>
        Last seen: ${pos.last_seen}`

      const stale = pos.last_seen ? (Date.now() - new Date(pos.last_seen).getTime() > 300000) : false
      const icon = cotMarkerIcon(pos.source, label, stale)
      const cotPopup = `<b>${label}</b><br/>
        IMEI: ${pos.imei}<br/>
        ${pos.lat.toFixed(6)}, ${pos.lon.toFixed(6)}<br/>
        Source: ${pos.source}<br/>
        Last seen: ${pos.last_seen}${stale ? '<br/><span style="color:#f59e0b">Stale</span>' : ''}`

      if (markers[key]) {
        markers[key].setLatLng([pos.lat, pos.lon])
        markers[key].setPopupContent(cotPopup)
        markers[key].setIcon(icon)
      } else {
        markers[key] = L.marker([pos.lat, pos.lon], { icon }).addTo(map).bindPopup(cotPopup)
        markers[key].on('click', () => onMarkerClick(key))
      }
    }
  } catch (e) {
    console.error('Failed to load positions:', e)
  }
}
</script>

<template>
  <div class="p-4 lg:p-6">
    <div class="flex items-center justify-between mb-4">
      <h1 class="text-2xl font-display font-bold">Position Map</h1>
      <div class="flex items-center gap-2">
        <label class="text-sm text-gray-400">Track range:</label>
        <select
          v-model="trackRange"
          @change="onRangeChange"
          class="bg-gray-800 text-gray-200 text-sm rounded px-2 py-1 border border-gray-600 focus:outline-none focus:border-cyan-500"
        >
          <option v-for="opt in rangeOptions" :key="opt.value" :value="opt.value">{{ opt.label }}</option>
        </select>
      </div>
    </div>
    <div ref="mapContainer" class="w-full rounded-lg overflow-hidden" style="height: calc(100vh - 140px);"></div>
  </div>
</template>
