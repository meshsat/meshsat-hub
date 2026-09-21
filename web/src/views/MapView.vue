<script setup>
import { ref, computed, onMounted, onUnmounted, watch } from 'vue'
import { useRoute } from 'vue-router'
import { createMap, setMapTheme, addSvgIcon, probeLocalArchive, maplibregl } from '../map/basemap'
import { useThemeStore } from '../stores/theme'
import { positions, bridges } from '../api/client'
import { ago } from '../utils/paths'
import Icon from '../components/Icon.vue'
import { useOpsStore } from '../stores/ops'

// The common operating picture: full bleed, every kit and device with a
// position, newest first in the list beside it. Markers follow the ISA-101
// rule the rest of the console does: shape says what a thing is (square kit,
// diamond satellite device, circle ground node), age fades it, Signal Orange
// marks the one you picked, and only an SOS is red.
const route = useRoute()
const theme = useThemeStore()
const ops = useOpsStore()
const mapContainer = ref(null)
const basemapMissing = ref(false)
const listOpen = ref(typeof window !== 'undefined' ? window.matchMedia('(min-width: 768px)').matches : true)
const query = ref('')

let map = null
let mapReady = false
let refreshInterval = null
let popup = null

const items = ref([])
const selectedKey = ref(null)
const show = ref({ kit: true, satellite: true, ground: true })
const trackRange = ref('24h')
let trackFeature = null

const RANGES = [
  { label: '24 h', value: '24h', ms: 24 * 3600000 },
  { label: '7 days', value: '7d', ms: 7 * 86400000 },
  { label: '30 days', value: '30d', ms: 30 * 86400000 },
]

// Marker colours come from the theme tokens, so the map re-themes with the
// rest of the console and no colour is written twice.
function token(name) {
  const v = getComputedStyle(document.documentElement).getPropertyValue(`--ms-${name}`).trim()
  return v ? `rgb(${v.split(/\s+/).join(',')})` : '#888'
}

function categoryOf(type) {
  if (type === 'bridge') return 'kit'
  if (/iridium|rockblock|globalstar|sbd|imt/.test(type || '')) return 'satellite'
  return 'ground'
}

// Age bands, not a stale flag: a satellite device that reports twice a day is
// not broken at the six-hour mark, so an old fix fades rather than warns.
function ageBand(ts) {
  if (!ts) return 'old'
  const h = (Date.now() - new Date(ts).getTime()) / 3600000
  return h < 1 ? 'fresh' : h < 24 ? 'day' : 'old'
}

function svgFor(shape, fill, stroke, opacity, dashed) {
  const s = `stroke="${stroke}" stroke-width="2.5"${dashed ? ' stroke-dasharray="4 3"' : ''}`
  const f = dashed ? 'none' : fill
  const body = shape === 'square' ? `<rect x="5" y="5" width="22" height="22" rx="3" fill="${f}" ${s}/>`
    : shape === 'diamond' ? `<polygon points="16,3 29,16 16,29 3,16" fill="${f}" ${s}/>`
      : shape === 'sos' ? `<circle cx="16" cy="16" r="13" fill="${fill}" ${s}/><path d="M10 10l12 12M22 10 10 22" stroke="${stroke}" stroke-width="3"/>`
        : `<circle cx="16" cy="16" r="11" fill="${f}" ${s}/>`
  return `<svg xmlns="http://www.w3.org/2000/svg" width="32" height="32" viewBox="0 0 32 32" opacity="${opacity}">${body}</svg>`
}

function iconFor(item, selected) {
  const shape = item.sos ? 'sos' : item.category === 'kit' ? 'square' : item.category === 'satellite' ? 'diamond' : 'circle'
  const band = ageBand(item.lastSeen)
  const id = `m-${shape}-${band}-${item.approx ? 'a' : 'x'}-${selected ? 's' : 'n'}-${theme.dark ? 'd' : 'l'}`
  if (map && !map.hasImage(id)) {
    const fill = item.sos ? token('error') : selected ? token('primary') : token('text')
    const stroke = item.sos ? token('bg') : selected ? token('bg') : token('bg')
    const opacity = selected || item.sos ? 1 : band === 'fresh' ? 1 : band === 'day' ? 0.75 : 0.45
    const outline = item.approx && !item.sos && !selected
    addSvgIcon(map, id, svgFor(shape, fill, outline ? token('text') : stroke, opacity, outline), [32, 32])
  }
  return id
}

const TYPE_WORDS = { bridge: 'Kit', iridium_imt: 'Iridium 9704', iridium_sbd: 'Iridium SBD', rockblock: 'RockBLOCK', meshtastic_node: 'Mesh node' }

function featureCollection() {
  const visible = items.value.filter((i) => show.value[i.category])
  return {
    type: 'FeatureCollection',
    features: visible.map((i) => ({
      type: 'Feature',
      properties: { key: i.key, icon: iconFor(i, i.key === selectedKey.value), label: i.name.length > 18 ? i.name.slice(0, 17) + '…' : i.name, sort: i.key === selectedKey.value ? 2 : i.sos ? 3 : 1 },
      geometry: { type: 'Point', coordinates: [i.lon, i.lat] },
    })),
  }
}

function setData() {
  if (!mapReady) return
  map.getSource('things')?.setData(featureCollection())
  map.getSource('track')?.setData({ type: 'FeatureCollection', features: trackFeature ? [trackFeature] : [] })
}

function labelColors() {
  return { color: token('text'), halo: token('bg') }
}

function addLayers() {
  const lc = labelColors()
  map.addSource('things', { type: 'geojson', data: { type: 'FeatureCollection', features: [] } })
  map.addSource('track', { type: 'geojson', data: { type: 'FeatureCollection', features: [] } })
  map.addLayer({ id: 'track-line', type: 'line', source: 'track', paint: { 'line-color': token('primary'), 'line-width': 2.5, 'line-opacity': 0.85 } })
  map.addLayer({
    id: 'things',
    type: 'symbol',
    source: 'things',
    layout: {
      'icon-image': ['get', 'icon'],
      'icon-size': 0.62,
      'icon-allow-overlap': true,
      'symbol-sort-key': ['get', 'sort'],
      'text-field': ['get', 'label'],
      'text-font': ['Noto Sans Regular'],
      'text-size': 12,
      'text-offset': [0, 1.35],
      'text-anchor': 'top',
      'text-optional': true,
    },
    paint: { 'text-color': lc.color, 'text-halo-color': lc.halo, 'text-halo-width': 1.6 },
  })
  map.on('click', 'things', (e) => { const f = e.features?.[0]; if (f) select(f.properties.key, false) })
  map.on('mouseenter', 'things', () => { map.getCanvas().style.cursor = 'pointer' })
  map.on('mouseleave', 'things', () => { map.getCanvas().style.cursor = '' })
  setData()
}

function escapeHtml(v) {
  return String(v ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]))
}

function popupHtml(i) {
  const status = i.category === 'kit' ? (i.online ? 'Connected' : 'Offline') : `Heard ${ago(i.lastSeen)}`
  return `<div class="min-w-[180px]">
    <div class="text-[13px] font-semibold">${escapeHtml(i.name)}</div>
    <div class="text-xs mt-0.5 opacity-70">${escapeHtml(TYPE_WORDS[i.type] || i.type || 'Device')}, ${escapeHtml(status)}</div>
    <div class="font-mono text-[11px] mt-2">${i.lat.toFixed(5)}, ${i.lon.toFixed(5)}</div>
    ${i.approx ? '<div class="text-xs mt-1 opacity-70">Approximate: an Iridium fix, accurate to a few kilometres.</div>' : ''}
    ${i.sos ? '<div class="text-xs mt-1 font-semibold">Unacknowledged SOS</div>' : i.sosFix ? '<div class="text-xs mt-1 opacity-70">This position came with an SOS that has been answered.</div>' : ''}
  </div>`
}

async function loadTrack(i) {
  trackFeature = null
  setData()
  if (i.category === 'kit') return
  const range = RANGES.find((r) => r.value === trackRange.value) || RANGES[0]
  try {
    const res = await positions.historyRange(i.id, new Date(Date.now() - range.ms).toISOString(), new Date().toISOString())
    const pts = Array.isArray(res) ? res : (res?.positions || [])
    const coords = pts.filter((p) => p.lat !== 0 || p.lon !== 0).map((p) => [p.lon, p.lat])
    if (selectedKey.value !== i.key || coords.length < 2) return
    trackFeature = { type: 'Feature', properties: {}, geometry: { type: 'LineString', coordinates: coords } }
    setData()
  } catch { /* a missing track leaves the marker as it is */ }
}

function select(key, fly = true) {
  const i = items.value.find((x) => x.key === key)
  if (!i) return
  selectedKey.value = key
  setData()
  popup?.remove()
  popup = new maplibregl.Popup({ closeButton: true, maxWidth: '280px', offset: 14 }).setLngLat([i.lon, i.lat]).setHTML(popupHtml(i)).addTo(map)
  popup.on('close', () => { if (selectedKey.value === key) { selectedKey.value = null; trackFeature = null; setData() } })
  if (fly) map.flyTo({ center: [i.lon, i.lat], zoom: Math.max(map.getZoom(), i.approx ? 10 : 13), speed: 1.4 })
  loadTrack(i)
  if (!window.matchMedia('(min-width: 768px)').matches) listOpen.value = false
}

function fitAll() {
  const vis = items.value.filter((i) => show.value[i.category])
  if (!vis.length || !mapReady) return
  const b = new maplibregl.LngLatBounds()
  vis.forEach((i) => b.extend([i.lon, i.lat]))
  map.fitBounds(b, { padding: { top: 80, bottom: 60, left: listOpen.value ? 360 : 60, right: 60 }, maxZoom: 13, duration: 600 })
}

async function refreshPositions(first = false) {
  const [devs, kits] = await Promise.all([positions.allLatest().catch(() => []), bridges.list().catch(() => [])])
  const next = []
  for (const p of Array.isArray(devs) ? devs : []) {
    if (!p.lat && !p.lon) continue
    // Red only while an SOS from this device is unacknowledged. A position
    // that once came with an SOS is history once somebody has answered it.
    const sosFix = p.source === 'sos' || p.source === 'emergency'
    const sosLive = sosFix && ops.alerts.some((a) => a.type === 'sos' && a.device_imei === p.imei)
    next.push({ key: 'dev:' + p.imei, id: p.imei, name: p.label || p.imei, type: p.type || '', category: categoryOf(p.type), lat: p.lat, lon: p.lon, lastSeen: p.last_seen, approx: p.source === 'iridium_cep', sos: sosLive, sosFix })
  }
  for (const b of Array.isArray(kits) ? kits : []) {
    if (!b.location_lat || !b.location_lon) continue
    next.push({ key: 'kit:' + b.bridge_id, id: b.bridge_id, name: b.cot_callsign || b.label || b.bridge_id, type: 'bridge', category: 'kit', lat: b.location_lat, lon: b.location_lon, lastSeen: b.last_seen, online: !!b.online, approx: false, sos: false })
  }
  next.sort((a, b) => Number(b.sos) - Number(a.sos) || new Date(b.lastSeen || 0) - new Date(a.lastSeen || 0))
  items.value = next
  setData()
  if (first) {
    const focus = route.query.focus ? String(route.query.focus) : ''
    const hit = focus && next.find((i) => i.id === focus)
    if (hit) select(hit.key)
    else fitAll()
  }
}

const listed = computed(() => {
  const q = query.value.trim().toLowerCase()
  return items.value.filter((i) => show.value[i.category] && (!q || `${i.name} ${i.id}`.toLowerCase().includes(q)))
})
const counts = computed(() => ({
  kit: items.value.filter((i) => i.category === 'kit').length,
  satellite: items.value.filter((i) => i.category === 'satellite').length,
  ground: items.value.filter((i) => i.category === 'ground').length,
}))

function toggle(cat) { show.value = { ...show.value, [cat]: !show.value[cat] }; setData() }
function onRange(v) {
  trackRange.value = v
  const i = items.value.find((x) => x.key === selectedKey.value)
  if (i) loadTrack(i)
}

onMounted(async () => {
  await probeLocalArchive()
  map = createMap({ container: mapContainer.value, center: [4.9, 52.37], zoom: 3, dark: theme.dark, onBasemapError: () => { basemapMissing.value = true } })
  map.on('load', () => { mapReady = true; addLayers(); refreshPositions(true) })
  refreshInterval = setInterval(() => refreshPositions(false), 30000)
})

watch(() => ops.alerts.length, () => { if (mapReady) refreshPositions(false) })

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
  <div class="relative h-full w-full overflow-hidden">
    <h1 class="sr-only">Map</h1>
    <div ref="mapContainer" class="absolute inset-0 bg-ms-bg2" data-testid="map" />

    <!-- List of everything with a position -->
    <aside v-if="listOpen" class="absolute z-10 top-3 left-3 max-h-[calc(100%-24px)] w-[min(320px,calc(100%-24px))] ms-panel shadow-2xl shadow-black/40 flex flex-col overflow-hidden" aria-label="Positions">
      <div class="px-3 pt-3 pb-2 border-b border-ms-border">
        <div class="flex items-center justify-between">
          <span class="ms-h2">On the map</span>
          <button class="ms-btn-ghost h-7 px-1.5" aria-label="Hide list" @click="listOpen = false"><Icon name="close" :size="16" /></button>
        </div>
        <label class="relative block mt-2">
          <span class="sr-only">Filter</span>
          <Icon name="search" :size="14" class="absolute left-2.5 top-2 text-ms-muted" />
          <input v-model="query" type="search" placeholder="Filter by name or id" class="ms-input w-full pl-8" />
        </label>
        <div class="flex gap-1.5 mt-2" role="group" aria-label="Show">
          <button v-for="c in [['kit', 'Kits'], ['satellite', 'Satellite'], ['ground', 'Mesh']]" :key="c[0]"
            class="h-7 px-2.5 rounded-full text-xs border transition-colors inline-flex items-center gap-1.5"
            :class="show[c[0]] ? 'border-ms-border-light text-ms-text bg-ms-well' : 'border-ms-border text-ms-muted line-through'"
            :aria-pressed="show[c[0]]" @click="toggle(c[0])">
            <span class="w-2 h-2 bg-current" :class="c[0] === 'kit' ? 'rounded-[1px]' : c[0] === 'satellite' ? 'rotate-45 rounded-[1px]' : 'rounded-full'" aria-hidden="true" />
            {{ c[1] }} <span class="ms-num text-ms-muted">{{ counts[c[0]] }}</span>
          </button>
        </div>
      </div>
      <ul class="min-h-0 overflow-y-auto tactical-scroll py-1">
        <li v-if="!listed.length" class="px-3 py-6 text-[13px] text-ms-muted">{{ items.length ? 'Nothing matches.' : 'Nothing has reported a position yet.' }}</li>
        <li v-for="i in listed" :key="i.key">
          <button class="w-full text-left px-3 py-2 flex items-center gap-2.5 transition-colors"
            :class="selectedKey === i.key ? 'bg-ms-well' : 'hover:bg-ms-well/60'" @click="select(i.key)">
            <span class="w-2.5 h-2.5 shrink-0" aria-hidden="true"
              :class="[
                i.category === 'kit' ? 'rounded-[2px]' : i.category === 'satellite' ? 'rotate-45 rounded-[1px]' : 'rounded-full',
                i.sos ? 'bg-ms-error' : selectedKey === i.key ? 'bg-ms-primary' : i.approx ? 'border border-ms-text' : 'bg-ms-text',
                !i.sos && selectedKey !== i.key && ageBand(i.lastSeen) === 'old' ? 'opacity-45' : '',
              ]" />
            <span class="min-w-0 flex-1">
              <span class="block text-[13px] truncate" :class="i.sos ? 'text-ms-error font-semibold' : 'text-ms-text'">{{ i.name }}</span>
              <span class="block text-xs text-ms-muted truncate">{{ TYPE_WORDS[i.type] || i.type || 'Device' }}<template v-if="i.approx">, approximate</template></span>
            </span>
            <span class="text-xs text-ms-muted whitespace-nowrap">{{ i.category === 'kit' ? (i.online ? 'Live' : ago(i.lastSeen)) : ago(i.lastSeen) }}</span>
          </button>
        </li>
      </ul>
      <div class="px-3 py-2 border-t border-ms-border flex items-center gap-2">
        <span class="text-xs text-ms-muted">Track</span>
        <div class="flex rounded-md border border-ms-border overflow-hidden" role="group" aria-label="Track length">
          <button v-for="r in RANGES" :key="r.value" class="h-7 px-2 text-xs transition-colors"
            :class="trackRange === r.value ? 'bg-ms-well text-ms-text' : 'text-ms-muted hover:text-ms-text'" :aria-pressed="trackRange === r.value" @click="onRange(r.value)">{{ r.label }}</button>
        </div>
        <button class="ms-btn-ghost h-7 text-xs ml-auto" @click="fitAll">Fit all</button>
      </div>
    </aside>
    <button v-else class="absolute z-10 top-3 left-3 ms-btn bg-ms-card shadow-lg shadow-black/30" @click="listOpen = true">
      <Icon name="layers" :size="15" />List <span class="ms-num text-ms-muted">{{ items.length }}</span>
    </button>

    <div v-if="basemapMissing" class="absolute z-10 top-3 right-3 max-w-sm rounded-lg border border-ms-warning/50 bg-ms-card px-3 py-2 text-xs text-ms-text shadow-lg">
      The basemap is unavailable, so positions are drawn on a blank backdrop. Markers and tracks still work.
    </div>
  </div>
</template>
