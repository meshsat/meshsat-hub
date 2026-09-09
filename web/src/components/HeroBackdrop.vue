<script setup>
// The meshsat.net hero, behind the Hub's sign-in (MESHSAT-995).
//
// Someone arrives from the marketing site, enrols on auth.meshsat.net and lands
// here. Those first two surfaces are photographic and this one was a flat
// colour, so it read as a different product. Same four frames, same order and
// the same pace as the site: six seconds a slide, a 1.5 s dissolve, a 24 s
// round trip. The numbers are the setInterval in
// meshsat-website/site/assets/js/main.js and the transition on .hero-slide, and
// this is the site's mechanism too, not a reimplementation of it.
//
// The photographs are copied into the bundle rather than loaded from
// meshsat.net. The Hub's CSP is `img-src 'self' data: blob:` and has been since
// MESHSAT-967 deliberately removed the last third-party image host; widening it
// for decoration on the sign-in page would be a poor trade. Vite fingerprints
// them, so they are cached until they change.
import { onMounted, onUnmounted, ref } from 'vue'
import fieldComms from '../assets/hero/field-comms.webp'
import satcom from '../assets/hero/satcom.webp'
import maritime from '../assets/hero/maritime.webp'
import sar from '../assets/hero/sar.webp'

// backgroundPosition matches the site: two of the four are framed low so the
// horizon sits where it does there.
const SLIDES = [
  { src: fieldComms, position: 'center 85%' },
  { src: satcom, position: 'center' },
  { src: maritime, position: 'center' },
  { src: sar, position: 'center 85%' },
]

const current = ref(0)
let timer = null

onMounted(() => {
  // Reduced motion stops the rotation rather than merely cutting between
  // frames, which is what the site does. A photograph changing every six
  // seconds behind a password field asks for more attention than a marketing
  // hero does.
  if (window.matchMedia?.('(prefers-reduced-motion: reduce)').matches) return
  timer = setInterval(() => {
    current.value = (current.value + 1) % SLIDES.length
  }, 6000)
})

onUnmounted(() => {
  if (timer) clearInterval(timer)
})
</script>

<template>
  <div class="hero-backdrop" aria-hidden="true">
    <div
      v-for="(slide, i) in SLIDES"
      :key="slide.src"
      class="hero-slide"
      :class="{ 'is-current': i === current }"
      :style="{ backgroundImage: `url(${slide.src})`, backgroundPosition: slide.position }"
    />
    <div class="hero-scrim" />
  </div>
</template>

<style scoped>
.hero-backdrop {
  position: absolute;
  inset: 0;
  overflow: hidden;
}

.hero-slide {
  position: absolute;
  inset: 0;
  background-size: cover;
  background-repeat: no-repeat;
  opacity: 0;
  transition: opacity 1.5s ease-in-out;
}

.hero-slide.is-current {
  opacity: 1;
}

/* One gradient for both themes: --ms-bg is Space Black in dark and Off White in
 * light, so the scrim follows the theme the way the site's hero overlay does.
 * Nothing but the opaque card sits on top of this, so it is tuned to let the
 * photographs read rather than to carry text. */
.hero-scrim {
  position: absolute;
  inset: 0;
  background: linear-gradient(
    135deg,
    rgb(var(--ms-bg) / 0.86) 0%,
    rgb(var(--ms-bg) / 0.76) 45%,
    rgb(var(--ms-bg) / 0.68) 100%
  );
}

@media (prefers-reduced-motion: reduce) {
  .hero-slide { transition: none; }
}

/* forced-colors keeps background-image but drops the gradient that made it
 * readable. The photograph is decorative; let the system colours work. */
@media (forced-colors: active) {
  .hero-backdrop { display: none; }
}
</style>
