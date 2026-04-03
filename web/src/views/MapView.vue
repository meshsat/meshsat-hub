<script setup>
import { ref, onMounted, onUnmounted } from 'vue'
import L from 'leaflet'
import { positions, bridges } from '../api/client'

const mapContainer = ref(null)
let map = null
let markers = {}
let markerColors = {}
let refreshInterval = null

const trackRange = ref('24h')
let activeTrackImei = null
let trackLayer = null

const rangeOptions = [
  { label: '24h', value: '24h' },
  { label: '7d', value: '7d' },
  { label: '30d', value: '30d' },
]

// TAK/CoT color mapping aligned with protocol.go device types and tactical design tokens
const typeColorMap = {
  iridium_sbd: '#A855F7',    // purple — satellite
  iridium_imt: '#A855F7',
  astrocast: '#818CF8',       // indigo — satellite
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
function svgSquare(color, label, stale) {
  const op = stale ? 0.45 : 1.0
  const lbl = shortLabel(label)
  const lblSvg = lbl ? `<text x="16" y="39" text-anchor="middle" fill="#fff" font-size="9" font-family="sans-serif" style="text-shadow:0 0 3px #000">${lbl}</text>` : ''
  return `<svg width="32" height="42" viewBox="0 0 32 42" opacity="${op}">` +
    `<rect x="3" y="3" width="26" height="26" fill="${color}" stroke="#fff" stroke-width="2" rx="2"/>` +
    lblSvg + `</svg>`
}

function svgDiamond(color, label, stale) {
  const op = stale ? 0.45 : 1.0
  const lbl = shortLabel(label)
  const lblSvg = lbl ? `<text x="16" y="39" text-anchor="middle" fill="#fff" font-size="9" font-family="sans-serif" style="text-shadow:0 0 3px #000">${lbl}</text>` : ''
  return `<svg width="32" height="42" viewBox="0 0 32 42" opacity="${op}">` +
    `<polygon points="16,2 30,16 16,30 2,16" fill="${color}" stroke="#fff" stroke-width="2"/>` +
    lblSvg + `</svg>`
}

function svgCircle(color, label, stale) {
  const op = stale ? 0.45 : 1.0
  const lbl = shortLabel(label)
  const lblSvg = lbl ? `<text x="16" y="39" text-anchor="middle" fill="#fff" font-size="9" font-family="sans-serif" style="text-shadow:0 0 3px #000">${lbl}</text>` : ''
  return `<svg width="32" height="42" viewBox="0 0 32 42" opacity="${op}">` +
    `<circle cx="16" cy="16" r="14" fill="${color}" stroke="#fff" stroke-width="2"/>` +
    lblSvg + `</svg>`
}

function svgSOS(label) {
  const lbl = shortLabel(label)
  const lblSvg = lbl ? `<text x="16" y="39" text-anchor="middle" fill="#fff" font-size="9" font-family="sans-serif" style="text-shadow:0 0 3px #000">${lbl}</text>` : ''
  return `<svg width="32" height="42" viewBox="0 0 32 42">` +
    `<circle cx="16" cy="16" r="14" fill="${sosColor}" stroke="#fff" stroke-width="2"/>` +
    `<line x1="8" y1="8" x2="24" y2="24" stroke="#fff" stroke-width="3"/>` +
    `<line x1="24" y1="8" x2="8" y2="24" stroke="#fff" stroke-width="3"/>` +
    lblSvg + `</svg>`
}

function shortLabel(label) {
  if (!label) return ''
  return label.length > 8 ? label.slice(-6) : label
}

function makeTakIcon(type, source, label, stale) {
  let svg
  if (source === 'sos' || source === 'emergency') {
    svg = svgSOS(label)
  } else if (type === 'bridge') {
    svg = svgSquare(bridgeColor, label, stale)
  } else if (type === 'iridium_sbd' || type === 'iridium_imt' || type === 'astrocast') {
    svg = svgDiamond(getColor(type, source), label, stale)
  } else {
    svg = svgCircle(getColor(type, source), label, stale)
  }
  return L.divIcon({
    html: svg,
    className: '',
    iconSize: [32, 42],
    iconAnchor: [16, 30],
    popupAnchor: [0, -30],
  })
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

function clearTrack() {
  if (trackLayer && map) {
    map.removeLayer(trackLayer)
    trackLayer = null
  }
}

async function loadTrack(key) {
  clearTrack()
  // Bridges don't have position history
  if (key.startsWith('bridge:')) {
    activeTrackImei = key
    return
  }
  const from = rangeToISO(trackRange.value)
  const to = new Date().toISOString()
  try {
    const pts = await positions.historyRange(key, from, to)
    if (!pts || pts.length === 0) {
      activeTrackImei = key
      return
    }
    const latlngs = pts
      .filter(p => p.lat !== 0 || p.lon !== 0)
      .map(p => [p.lat, p.lon])
    if (latlngs.length < 2) {
      activeTrackImei = key
      return
    }
    trackLayer = L.polyline(latlngs, {
      color: markerColors[key] || defaultColor,
      weight: 2,
      opacity: 0.6,
    }).addTo(map)
    activeTrackImei = key
  } catch (e) {
    console.error('Failed to load track:', e)
  }
}

function onMarkerClick(key) {
  if (activeTrackImei === key) {
    clearTrack()
    activeTrackImei = null
  } else {
    loadTrack(key)
  }
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

function upsertMarker(key, lat, lon, type, source, label, lastSeen) {
  const stale = isStale(lastSeen)
  const icon = makeTakIcon(type, source, label, stale)
  const color = type === 'bridge' ? bridgeColor : getColor(type, source)
  markerColors[key] = color

  if (markers[key]) {
    markers[key].setLatLng([lat, lon])
    markers[key].setIcon(icon)
  } else {
    markers[key] = L.marker([lat, lon], { icon }).addTo(map)
    markers[key].on('click', () => onMarkerClick(key))
  }
  return { stale }
}

async function refreshPositions() {
  try {
    const [deviceData, bridgeData] = await Promise.all([
      positions.allLatest(),
      bridges.list().catch(() => []),
    ])

    // Device positions
    for (const pos of deviceData) {
      if (pos.lat === 0 && pos.lon === 0) continue
      const key = pos.imei
      const label = pos.label || pos.imei
      const { stale } = upsertMarker(key, pos.lat, pos.lon, pos.type || '', pos.source, label, pos.last_seen)

      const staleTag = stale ? '<br/><span style="color:#f59e0b">&#9679; Stale</span>' : ''
      const typeTag = pos.type ? `Type: ${pos.type}<br/>` : ''
      const popup = `<b>${label}</b><br/>
        IMEI: ${pos.imei}<br/>
        ${typeTag}${pos.lat.toFixed(6)}, ${pos.lon.toFixed(6)}<br/>
        Source: ${pos.source}<br/>
        Last seen: ${pos.last_seen}${staleTag}`
      markers[key].bindPopup(popup)
    }

    // Bridge fleet positions (square markers)
    const fleetList = Array.isArray(bridgeData) ? bridgeData : []
    for (const b of fleetList) {
      if (!b.location_lat || !b.location_lon) continue
      if (b.location_lat === 0 && b.location_lon === 0) continue
      const key = `bridge:${b.bridge_id}`
      const label = b.label || b.bridge_id
      upsertMarker(key, b.location_lat, b.location_lon, 'bridge', '', label, null)

      const status = b.online
        ? '<span style="color:#34d399">&#9679; Online</span>'
        : '<span style="color:#f87171">&#9679; Offline</span>'
      const popup = `<b>&#9632; ${label}</b><br/>
        Bridge: ${b.bridge_id}<br/>
        ${status}<br/>
        ${b.location_lat.toFixed(6)}, ${b.location_lon.toFixed(6)}` +
        (b.version ? `<br/>Version: ${b.version}` : '')
      markers[key].bindPopup(popup)
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
      <div class="flex items-center gap-4">
        <!-- TAK legend -->
        <div class="hidden md:flex items-center gap-3 text-xs text-gray-400">
          <span class="flex items-center gap-1">
            <svg width="12" height="12"><rect x="1" y="1" width="10" height="10" rx="1" fill="#22D3EE" stroke="#fff" stroke-width="1"/></svg>
            Bridge
          </span>
          <span class="flex items-center gap-1">
            <svg width="12" height="12"><polygon points="6,1 11,6 6,11 1,6" fill="#A855F7" stroke="#fff" stroke-width="1"/></svg>
            Satellite
          </span>
          <span class="flex items-center gap-1">
            <svg width="12" height="12"><circle cx="6" cy="6" r="5" fill="#06B6D4" stroke="#fff" stroke-width="1"/></svg>
            Ground
          </span>
        </div>
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
    </div>
    <div ref="mapContainer" class="w-full rounded-lg overflow-hidden" style="height: calc(100vh - 140px);"></div>
  </div>
</template>
