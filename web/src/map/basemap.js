// Self-hosted vector basemap for the Hub maps (MESHSAT-967).
//
// MapLibre GL renders a Protomaps vector basemap read straight out of one
// PMTiles archive that the Hub streams from its own object store, so a
// browser showing device positions asks no third-party tile host for
// anything: the areas an operator looks at, which are roughly where the
// devices are, never leave our infrastructure. Glyph ranges and sprite
// sheets come from the same place.
//
// protomaps-themes-base is the v4 theme generator and is deprecated upstream
// in favour of @protomaps/basemaps v5. v5 renders the v5 tile schema; the
// archive we ship is a v4 build (basemap version 4.15.2), so v4 is the
// matching package. Both are BSD-3-Clause.
//
// maplibre-gl is pinned to 5.x on purpose: with 6.8.0 the vector source
// resolves its TileJSON through the pmtiles protocol and then never requests
// a tile, so the map renders empty with no error (checked 2026-09-08, pmtiles
// 4.5.0). Re-test before moving to 6.
import * as maplibregl from 'maplibre-gl'
import { Protocol } from 'pmtiles'
import { layersWithCustomTheme, namedTheme } from 'protomaps-themes-base'
import 'maplibre-gl/dist/maplibre-gl.css'

const ARCHIVE = '/basemap/basemap.pmtiles'
const LOCAL_ARCHIVE = '/basemap/local.pmtiles'
const ASSETS = '/basemap/assets'
// Street geometry lives at zoom 13 in this schema and street names at 15, so a
// world archive that stayed small enough to host could never show either. The
// world archive carries the globe to zoom 11, and a second, deeper archive
// carries the area the fleet operates in the rest of the way. Its layers draw
// on top from this zoom up; outside its coverage it simply has no tiles and the
// world layers below stay visible.
const LOCAL_FROM_ZOOM = 11
export const ATTRIBUTION =
  '&copy; <a href="https://www.openstreetmap.org/copyright" target="_blank" rel="noopener">OpenStreetMap</a>, ' +
  '<a href="https://protomaps.com" target="_blank" rel="noopener">Protomaps</a>'

let protocolRegistered = false

function registerProtocol() {
  if (protocolRegistered) return
  maplibregl.addProtocol('pmtiles', new Protocol().tile)
  protocolRegistered = true
}

// token reads one brand CSS variable ("20 18 15") as a hex colour so the map
// stays on the palette the rest of the SPA uses.
function token(name, fallback) {
  const raw = getComputedStyle(document.documentElement).getPropertyValue(name).trim()
  const parts = raw.split(/[\s,]+/).map(Number)
  if (parts.length < 3 || parts.some(Number.isNaN)) return fallback
  return '#' + parts.slice(0, 3).map((v) => Math.max(0, Math.min(255, v)).toString(16).padStart(2, '0')).join('')
}

// mapTheme takes the upstream palette and pulls the large flat areas onto the
// MeshSat tokens; the hundred road, landuse and label colours stay upstream's,
// which are already tuned for legibility at every zoom.
function mapTheme(dark) {
  const base = namedTheme(dark ? 'dark' : 'light')
  const bg = token('--ms-bg', dark ? '#14120f' : '#f7f5f2')
  const well = token('--ms-well', dark ? '#1a1714' : '#efebe5')
  return {
    ...base,
    background: bg,
    earth: bg,
    water: dark ? '#1b2530' : '#dbe4ea',
    park_a: well,
    park_b: well,
    wood_a: well,
    wood_b: well,
  }
}

// hasLocalArchive is resolved once at startup: the route only exists when a
// deeper archive is configured, so a deployment without one still gets a map.
let localArchive = null
export async function probeLocalArchive() {
  if (localArchive !== null) return localArchive
  try {
    const r = await fetch(LOCAL_ARCHIVE, { method: 'HEAD' })
    localArchive = r.ok
  } catch {
    localArchive = false
  }
  return localArchive
}

export function basemapStyle(dark) {
  const theme = mapTheme(dark)
  const sources = {
    protomaps: {
      type: 'vector',
      url: `pmtiles://${window.location.origin}${ARCHIVE}`,
      attribution: ATTRIBUTION,
    },
  }
  const layers = layersWithCustomTheme('protomaps', theme, 'en')
  if (localArchive) {
    sources.protomaps_local = {
      type: 'vector',
      url: `pmtiles://${window.location.origin}${LOCAL_ARCHIVE}`,
    }
    // The same theme against the deeper source, drawn on top from the zoom
    // where the world archive runs out. Ids must not collide with the world
    // set, and the background layer belongs to neither source.
    for (const layer of layersWithCustomTheme('protomaps_local', theme, 'en')) {
      if (!layer.source) continue
      layers.push({
        ...layer,
        id: `${layer.id}__local`,
        minzoom: Math.max(layer.minzoom ?? 0, LOCAL_FROM_ZOOM),
      })
    }
  }
  return {
    version: 8,
    glyphs: `${window.location.origin}${ASSETS}/fonts/{fontstack}/{range}.pbf`,
    sprite: `${window.location.origin}${ASSETS}/sprites/v4/${dark ? 'dark' : 'light'}`,
    sources,
    layers,
  }
}

// createMap returns a MapLibre map on the self-hosted style. onBasemapError is
// called once if the archive cannot be read: the caller keeps its own data
// layers and tells the operator the backdrop is missing, rather than falling
// back to a public tile host.
export function createMap({ container, center = [4.9, 52.37], zoom = 3, dark = true, onBasemapError = null }) {
  registerProtocol()
  const map = new maplibregl.Map({
    container,
    style: basemapStyle(dark),
    center,
    zoom,
    attributionControl: { compact: true },
    // The world archive reaches zoom 11 and the deeper regional one 15;
    // MapLibre overzooms beyond whichever applies, so the geometry stays crisp.
    maxZoom: 18,
  })
  map.addControl(new maplibregl.NavigationControl({ showCompass: false }), 'top-right')
  map.addControl(new maplibregl.ScaleControl({ maxWidth: 120, unit: 'metric' }))
  // A silent map is the hardest kind to debug from a field report, so every
  // distinct style or source failure is warned about once.
  const seen = new Set()
  let reported = false
  map.on('error', (e) => {
    const msg = String(e?.error?.message || e?.error || '')
    if (!seen.has(msg)) {
      seen.add(msg)
      console.warn('map: ' + msg)
    }
    if (!onBasemapError || reported || !/pmtiles|basemap|\/basemap\//i.test(msg)) return
    reported = true
    onBasemapError(msg)
  })
  return map
}

// setMapTheme swaps the basemap palette in place, keeping the camera. The
// caller re-adds its own sources and layers on the returned promise, because
// setStyle drops everything that is not part of the style.
export function setMapTheme(map, dark) {
  map.setStyle(basemapStyle(dark), { diff: false })
  return new Promise((resolve) => map.once('styledata', resolve))
}

// addSvgIcon registers an inline SVG as a map image, so the TAK marker shapes
// are drawn by the GPU like any other symbol.
export function addSvgIcon(map, id, svg, size) {
  return new Promise((resolve) => {
    if (map.hasImage(id)) {
      resolve()
      return
    }
    const img = new Image(size[0] * 2, size[1] * 2)
    img.onload = () => {
      if (!map.hasImage(id)) map.addImage(id, img, { pixelRatio: 2 })
      resolve()
    }
    img.onerror = () => resolve()
    img.src = 'data:image/svg+xml;charset=utf-8,' + encodeURIComponent(svg)
  })
}

export { maplibregl }
