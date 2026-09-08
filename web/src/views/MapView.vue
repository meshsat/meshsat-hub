<script setup>
import { ref, onMounted, onUnmounted, watch } from 'vue'
import { createMap, setMapTheme, addSvgIcon, maplibregl } from '../map/basemap'
import { useThemeStore } from '../stores/theme'
import { positions, bridges } from '../api/client'

const mapContainer = ref(null)
const basemapMissing = ref(false)
const theme = useThemeStore()

let map = null
let mapReady = false
let refreshInterval = null
let popup = null

// Feature state, kept outside Vue: the map owns the rendering.
let features = []
let trackFeature = null
let markerColors = {}

// CoT type filter — all visible by default
const filterBridge = ref(true)
const filterSatellite = ref(true)
const filterGround = ref(true)

const trackRange = ref('24h')
let activeTrackKey = null

const rangeOptions = [
  { label: '24h', value: '24h' },
  { label: '7d', value: '7d' },
  { label: '30d', value: '30d' },
]

// TAK/CoT color mapping aligned with protocol.go device types and tactical design tokens
const typeColorMap = {
  iridium_sbd: '#A855F7',    // purple — satellite
  iridium_imt: '#A855F7',
  meshtastic_node: '#06B6D4', // cyan — mesh/LoRa
  zigbee: '#06B6D4',
  aprs: '#10B981',            // emerald
  cellular: '#F97316',        // orange
}
const bridgeColor = '#22D3EE'  // cyan-400 — infrastructure
const defaultColor = '#22D3EE'
const cepColor = '#F59E0B'     // amber — low-accuracy satellite fix
const sosColor = '#EF4444'     // red — emergency

function getColor(type, source) {
  if (source === 'iridium_cep') return cepColor
  return typeColorMap[type] || defaultColor
}

// TAK/CoT-compliant SVG marker shapes:
//   Bridge  → square (MIL-STD-2525: infrastructure/installation)
//   Sat modem → diamond (sensor/equipment)
//   Mesh/ground → circle (friendly ground unit)
//   Emergency → circle with X
function svgSquare(color, op) {
  return `<svg xmlns="http://www.w3.org/2000/svg" width="32" height="32" viewBox="0 0 32 32" opacity="${op}">` +
    `<rect x="3" y="3" width="26" height="26" fill="${color}" stroke="#fff" stroke-width="2" rx="2"/></svg>`
}

function svgDiamond(color, op) {
  return `<svg xmlns="http://www.w3.org/2000/svg" width="32" height="32" viewBox="0 0 32 32" opacity="${op}">` +
    `<polygon points="16,2 30,16 16,30 2,16" fill="${color}" stroke="#fff" stroke-width="2"/></svg>`
}

function svgCircle(color, op) {
  return `<svg xmlns="http://www.w3.org/2000/svg" width="32" height="32" viewBox="0 0 32 32" opacity="${op}">` +
    `<circle cx="16" cy="16" r="14" fill="${color}" stroke="#fff" stroke-width="2"/></svg>`
}

function svgSOS() {
  return `<svg xmlns="http://www.w3.org/2000/svg" width="32" height="32" viewBox="0 0 32 32">` +
    `<circle cx="16" cy="16" r="14" fill="${sosColor}" stroke="#fff" stroke-width="2"/>` +
    `<line x1="8" y1="8" x2="24" y2="24" stroke="#fff" stroke-width="3"/>` +
    `<line x1="24" y1="8" x2="8" y2="24" stroke="#fff" stroke-width="3"/></svg>`
}

// iconFor names one registered map image per shape, colour and staleness, and
// registers it the first time it is asked for.
function iconFor(type, source, stale) {
  const emergency = source === 'sos' || source === 'emergency'
  const color = type === 'bridge' ? bridgeColor : getColor(type, source)
  let shape = 'circle'
  if (emergency) shape = 'sos'
  else if (type === 'bridge') shape = 'square'
  else if (type === 'iridium_sbd' || type === 'iridium_imt') shape = 'diamond'

  const id = `${shape}-${color.replace('#', '')}-${stale ? 'stale' : 'live'}`
  if (map && !map.hasImage(id)) {
    const op = stale ? 0.45 : 1
    const svg = shape === 'sos' ? svgSOS()
      : shape === 'square' ? svgSquare(color, op)
        : shape === 'diamond' ? svgDiamond(color, op) : svgCircle(color, op)
    addSvgIcon(map, id, svg, [32, 32])
  }
  return { icon: id, color }
}

function isStale(lastSeen) {
  if (!lastSeen) return false
  return Date.now() - new Date(lastSeen).getTime() > 300000 // 5 min
}

function rangeToISO(range) {
  const now = new Date()
  const ms = { '24h': 24 * 3600000, '7d': 7 * 86400000, '30d': 30 * 86400000 }
  return new Date(now.getTime() - (ms[range] || ms['24h'])).toISOString()
}

function markerCategory(type) {
  if (type === 'bridge') return 'bridge'
  if (type === 'iridium_sbd' || type === 'iridium_imt') return 'satellite'
  return 'ground'
}

function visibleCategories() {
  const out = []
  if (filterBridge.value) out.push('bridge')
  if (filterSatellite.value) out.push('satellite')
  if (filterGround.value) out.push('ground')
  return out
}

function applyFilters() {
  if (!mapReady) return
  map.setFilter('device-markers', ['in', ['get', 'category'], ['literal', visibleCategories()]])
}

function setData() {
  if (!mapReady) return
  map.getSource('devices')?.setData({ type: 'FeatureCollection', features })
  map.getSource('track')?.setData(
    trackFeature ? { type: 'FeatureCollection', features: [trackFeature] } : { type: 'FeatureCollection', features: [] },
  )
}

function clearTrack() {
  trackFeature = null
  setData()
}

async function loadTrack(key) {
  clearTrack()
  // Bridges don't have position history
  if (key.startsWith('bridge:')) {
    activeTrackKey = key
    return
  }
  const from = rangeToISO(trackRange.value)
  const to = new Date().toISOString()
  try {
    // The endpoint answers {positions: [...]}; older callers assumed a bare
    // array and threw, which is why tracks never drew (MESHSAT-967).
    const res = await positions.historyRange(key, from, to)
    const pts = Array.isArray(res) ? res : (res?.positions || [])
    activeTrackKey = key
    if (pts.length === 0) return
    const coords = pts.filter((p) => p.lat !== 0 || p.lon !== 0).map((p) => [p.lon, p.lat])
    if (coords.length < 2) return
    trackFeature = {
      type: 'Feature',
      properties: { color: markerColors[key] || defaultColor },
      geometry: { type: 'LineString', coordinates: coords },
    }
    setData()
  } catch (e) {
    console.error('Failed to load track:', e)
  }
}

function onMarkerClick(key) {
  if (activeTrackKey === key) {
    clearTrack()
    activeTrackKey = null
  } else {
    loadTrack(key)
  }
}

function onRangeChange() {
  if (activeTrackKey) loadTrack(activeTrackKey)
}

function escapeHtml(v) {
  return String(v ?? '').replace(/[&<>"']/g, (c) => (
    { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
  ))
}

function addLayers() {
  map.addSource('devices', { type: 'geojson', data: { type: 'FeatureCollection', features: [] } })
  map.addSource('track', { type: 'geojson', data: { type: 'FeatureCollection', features: [] } })
  map.addLayer({
    id: 'device-track',
    type: 'line',
    source: 'track',
    paint: { 'line-color': ['get', 'color'], 'line-width': 2, 'line-opacity': 0.6 },
  })
  map.addLayer({
    id: 'device-markers',
    type: 'symbol',
    source: 'devices',
    layout: {
      'icon-image': ['get', 'icon'],
      'icon-size': 0.5,
      'icon-allow-overlap': true,
      'text-field': ['get', 'shortLabel'],
      'text-font': ['Noto Sans Regular'],
      'text-size': 11,
      'text-offset': [0, 1.3],
      'text-anchor': 'top',
      'text-allow-overlap': false,
      'text-optional': true,
    },
    paint: {
      'text-color': theme.dark ? '#F2EDE6' : '#1A1714',
      'text-halo-color': theme.dark ? '#14120F' : '#F7F5F2',
      'text-halo-width': 1.5,
    },
  })
  map.on('click', 'device-markers', (e) => {
    const f = e.features?.[0]
    if (!f) return
    popup?.remove()
    popup = new maplibregl.Popup({ closeButton: true, maxWidth: '280px' })
      .setLngLat(f.geometry.coordinates)
      .setHTML(f.properties.popup)
      .addTo(map)
    onMarkerClick(f.properties.key)
  })
  map.on('mouseenter', 'device-markers', () => { map.getCanvas().style.cursor = 'pointer' })
  map.on('mouseleave', 'device-markers', () => { map.getCanvas().style.cursor = '' })
  applyFilters()
  setData()
}

function shortLabel(label) {
  if (!label) return ''
  return label.length > 10 ? label.slice(-8) : label
}

function makeFeature(key, lon, lat, type, source, label, lastSeen, popupHtml) {
  const stale = isStale(lastSeen)
  const { icon, color } = iconFor(type, source, stale)
  markerColors[key] = color
  return {
    type: 'Feature',
    properties: {
      key,
      icon,
      category: markerCategory(type),
      shortLabel: shortLabel(label),
      popup: popupHtml,
    },
    geometry: { type: 'Point', coordinates: [lon, lat] },
  }
}

async function refreshPositions() {
  try {
    const [deviceData, bridgeData] = await Promise.all([
      positions.allLatest(),
      bridges.list().catch(() => []),
    ])
    const next = []

    // Device positions
    for (const pos of deviceData) {
      if (pos.lat === 0 && pos.lon === 0) continue
      const label = pos.label || pos.imei
      const stale = isStale(pos.last_seen)
      const staleTag = stale ? '<br/><span style="color:#f59e0b">&#9679; Stale</span>' : ''
      const typeTag = pos.type ? `Type: ${escapeHtml(pos.type)}<br/>` : ''
      const popupHtml = `<b>${escapeHtml(label)}</b><br/>
        IMEI: ${escapeHtml(pos.imei)}<br/>
        ${typeTag}${pos.lat.toFixed(6)}, ${pos.lon.toFixed(6)}<br/>
        Source: ${escapeHtml(pos.source)}<br/>
        Last seen: ${escapeHtml(pos.last_seen)}${staleTag}`
      next.push(makeFeature(pos.imei, pos.lon, pos.lat, pos.type || '', pos.source, label, pos.last_seen, popupHtml))
    }

    // Bridge fleet positions (square markers)
    const fleetList = Array.isArray(bridgeData) ? bridgeData : []
    for (const b of fleetList) {
      if (!b.location_lat || !b.location_lon) continue
      if (b.location_lat === 0 && b.location_lon === 0) continue
      const key = `bridge:${b.bridge_id}`
      const label = b.label || b.bridge_id
      const status = b.online
        ? '<span style="color:#34d399">&#9679; Online</span>'
        : '<span style="color:#f87171">&#9679; Offline</span>'
      const popupHtml = `<b>&#9632; ${escapeHtml(label)}</b><br/>
        Bridge: ${escapeHtml(b.bridge_id)}<br/>
        ${status}<br/>
        ${b.location_lat.toFixed(6)}, ${b.location_lon.toFixed(6)}` +
        (b.version ? `<br/>Version: ${escapeHtml(b.version)}` : '')
      next.push(makeFeature(key, b.location_lon, b.location_lat, 'bridge', '', label, null, popupHtml))
    }

    features = next
    setData()
  } catch (e) {
    console.error('Failed to load positions:', e)
  }
}

onMounted(async () => {
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
    refreshPositions()
  })
  refreshInterval = setInterval(refreshPositions, 30000)
})

watch(() => theme.dark, async (dark) => {
  if (!map) return
  mapReady = false
  await setMapTheme(map, dark)
  mapReady = true
  addLayers()
})

onUnmounted(() => {
  if (refreshInterval) clearInterval(refreshInterval)
  popup?.remove()
  if (map) map.remove()
})
</script>

<template>
  <div class="p-4 lg:p-6">
    <div class="flex flex-wrap items-center justify-between gap-3 mb-4">
      <h1 class="text-2xl font-display font-bold">Position Map</h1>
      <div class="flex flex-wrap items-center gap-4">
        <!-- TAK CoT filter + legend -->
        <div class="hidden md:flex items-center gap-3 text-xs">
          <button @click="filterBridge = !filterBridge; applyFilters()"
            class="flex items-center gap-1 px-1.5 py-0.5 rounded transition-colors"
            :class="filterBridge ? 'text-cyan-400 bg-cyan-900/20' : 'text-ms-muted line-through'">
            <svg width="12" height="12"><rect x="1" y="1" width="10" height="10" rx="1" :fill="filterBridge ? '#22D3EE' : '#4B5563'" stroke="#fff" stroke-width="1"/></svg>
            Bridge
          </button>
          <button @click="filterSatellite = !filterSatellite; applyFilters()"
            class="flex items-center gap-1 px-1.5 py-0.5 rounded transition-colors"
            :class="filterSatellite ? 'text-purple-400 bg-purple-900/20' : 'text-ms-muted line-through'">
            <svg width="12" height="12"><polygon points="6,1 11,6 6,11 1,6" :fill="filterSatellite ? '#A855F7' : '#4B5563'" stroke="#fff" stroke-width="1"/></svg>
            Satellite
          </button>
          <button @click="filterGround = !filterGround; applyFilters()"
            class="flex items-center gap-1 px-1.5 py-0.5 rounded transition-colors"
            :class="filterGround ? 'text-cyan-400 bg-cyan-900/20' : 'text-ms-muted line-through'">
            <svg width="12" height="12"><circle cx="6" cy="6" r="5" :fill="filterGround ? '#06B6D4' : '#4B5563'" stroke="#fff" stroke-width="1"/></svg>
            Ground
          </button>
        </div>
        <div class="flex items-center gap-2">
          <label for="track-range" class="text-sm text-gray-400">Track range:</label>
          <select
            id="track-range"
            v-model="trackRange"
            @change="onRangeChange"
            class="bg-gray-800 text-gray-200 text-sm rounded px-2 py-1 border border-gray-600 focus:outline-none focus:border-cyan-500"
          >
            <option v-for="opt in rangeOptions" :key="opt.value" :value="opt.value">{{ opt.label }}</option>
          </select>
        </div>
      </div>
    </div>
    <div v-if="basemapMissing"
      class="mb-3 rounded border border-amber-700/50 bg-amber-900/20 text-amber-200 text-xs px-3 py-2">
      The self-hosted basemap is unavailable, so positions are drawn on an empty backdrop. Tracks, markers and filters still work.
    </div>
    <div ref="mapContainer" class="w-full rounded-lg overflow-hidden bg-ms-bg2" style="height: calc(100vh - 140px);"></div>
  </div>
</template>
