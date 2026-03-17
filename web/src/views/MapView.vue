<script setup>
import { ref, onMounted, onUnmounted } from 'vue'
import L from 'leaflet'
import { positions } from '../api/client'

const mapContainer = ref(null)
let map = null
let markers = {}
let refreshInterval = null

onMounted(async () => {
  map = L.map(mapContainer.value).setView([52.37, 4.90], 4)

  L.tileLayer('https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png', {
    attribution: '&copy; OpenStreetMap &copy; CARTO',
    maxZoom: 19,
  }).addTo(map)

  await refreshPositions()
  refreshInterval = setInterval(refreshPositions, 30000) // refresh every 30s
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

      if (markers[key]) {
        markers[key].setLatLng([pos.lat, pos.lon])
        markers[key].setPopupContent(popup)
      } else {
        markers[key] = L.circleMarker([pos.lat, pos.lon], {
          radius: 8,
          fillColor: pos.source === 'iridium_cep' ? '#f59e0b' : '#22d3ee',
          color: '#fff',
          weight: 2,
          opacity: 1,
          fillOpacity: 0.8,
        }).addTo(map).bindPopup(popup)
      }
    }
  } catch (e) {
    console.error('Failed to load positions:', e)
  }
}
</script>

<template>
  <div>
    <h1 style="font-size: 1.25rem; font-weight: bold; margin-bottom: 0.5rem;">Position Map</h1>
    <div ref="mapContainer" style="height: calc(100vh - 120px); border-radius: 8px; overflow: hidden;"></div>
  </div>
</template>
