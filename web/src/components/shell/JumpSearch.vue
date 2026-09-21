<script setup>
import { ref, computed, watch, nextTick, onMounted, onUnmounted } from 'vue'
import { useRouter } from 'vue-router'
import { useAuthStore } from '../../stores/auth'
import { useOpsStore } from '../../stores/ops'
import { visibleGroups } from '../../nav'
import Icon from '../Icon.vue'

// Jump to any page, kit or device from the keyboard: "/" or Ctrl/Cmd-K.
// Kits and devices come from the store the shell already polls, so typing
// costs no request.
const router = useRouter()
const auth = useAuthStore()
const ops = useOpsStore()
const open = ref(false)
const q = ref('')
const sel = ref(0)
const input = ref(null)

const pages = computed(() => visibleGroups(auth).flatMap((g) => g.items.map((i) => ({
  key: 'p:' + i.to, kind: 'Page', label: i.label, hint: g.label, icon: i.icon, hay: `${i.label} ${g.label} ${i.keywords || ''}`.toLowerCase(), to: i.to,
}))))
const kits = computed(() => ops.kits.map((k) => ({
  key: 'k:' + k.id, kind: 'Kit', label: k.name, hint: k.id, icon: 'kits', mono: true, hay: `${k.name} ${k.id}`.toLowerCase(), to: { name: 'fleet', query: { kit: k.id } },
})))
const devs = computed(() => ops.devices.map((d) => ({
  key: 'd:' + d.imei, kind: 'Device', label: d.label || d.imei, hint: d.imei, icon: 'devices', mono: true, hay: `${d.label || ''} ${d.imei} ${d.type || ''}`.toLowerCase(), to: { name: 'deviceDetail', params: { imei: d.imei } },
})))

const results = computed(() => {
  const t = q.value.trim().toLowerCase()
  const all = [...pages.value, ...kits.value, ...devs.value]
  if (!t) return pages.value.slice(0, 8)
  const words = t.split(/\s+/)
  return all.filter((r) => words.every((w) => r.hay.includes(w))).slice(0, 12)
})

watch(q, () => { sel.value = 0 })

async function show() {
  open.value = true
  q.value = ''
  await nextTick()
  input.value?.focus()
}
function hide() { open.value = false }
function pick(r) {
  if (!r) return
  hide()
  router.push(r.to)
}
function onInputKey(e) {
  if (e.key === 'ArrowDown') { e.preventDefault(); sel.value = Math.min(sel.value + 1, results.value.length - 1) }
  else if (e.key === 'ArrowUp') { e.preventDefault(); sel.value = Math.max(sel.value - 1, 0) }
  else if (e.key === 'Enter') { e.preventDefault(); pick(results.value[sel.value]) }
  else if (e.key === 'Escape') { hide() }
}
function onGlobalKey(e) {
  const tag = (e.target && e.target.tagName) || ''
  const typing = /INPUT|TEXTAREA|SELECT/.test(tag) || e.target?.isContentEditable
  if ((e.key === 'k' || e.key === 'K') && (e.metaKey || e.ctrlKey)) { e.preventDefault(); open.value ? hide() : show() }
  else if (e.key === '/' && !typing && !open.value) { e.preventDefault(); show() }
}
onMounted(() => document.addEventListener('keydown', onGlobalKey))
onUnmounted(() => document.removeEventListener('keydown', onGlobalKey))
defineExpose({ show })
</script>

<template>
  <button type="button" data-testid="jump"
    class="flex items-center gap-2 h-8 w-full max-w-[360px] px-2.5 rounded-md border border-ms-border bg-ms-bg text-[13px] text-ms-muted hover:border-ms-border-light hover:text-ms-text2 transition-colors"
    @click="show">
    <Icon name="search" :size="15" />
    <span class="truncate">Search pages, kits and devices</span>
    <kbd class="ml-auto hidden lg:inline-flex items-center h-5 px-1.5 rounded border border-ms-border text-[11px] font-mono text-ms-muted">/</kbd>
  </button>

  <Teleport to="body">
    <div v-if="open" class="fixed inset-0 z-[60] flex items-start justify-center pt-[12vh] px-3" @mousedown.self="hide">
      <div class="absolute inset-0 bg-ms-bg/70 backdrop-blur-[2px]" aria-hidden="true" @mousedown="hide" />
      <div role="dialog" aria-label="Search" class="relative w-full max-w-[560px] ms-panel shadow-2xl shadow-black/50 overflow-hidden">
        <div class="flex items-center gap-2.5 px-4 h-12 border-b border-ms-border">
          <Icon name="search" :size="17" class="text-ms-muted" />
          <input ref="input" v-model="q" type="text" placeholder="Type a page, kit name, IMEI"
            class="flex-1 bg-transparent text-[15px] text-ms-text placeholder:text-ms-muted focus:outline-none"
            role="combobox" aria-expanded="true" aria-controls="jump-results" :aria-activedescendant="results[sel] ? 'jump-' + sel : undefined"
            @keydown="onInputKey" />
          <kbd class="h-5 px-1.5 rounded border border-ms-border text-[11px] font-mono text-ms-muted inline-flex items-center">Esc</kbd>
        </div>
        <ul id="jump-results" role="listbox" class="max-h-[50vh] overflow-y-auto tactical-scroll py-1.5">
          <li v-if="!results.length" class="px-4 py-6 text-sm text-ms-muted">Nothing matches "{{ q }}".</li>
          <li v-for="(r, i) in results" :id="'jump-' + i" :key="r.key" role="option" :aria-selected="i === sel"
            class="mx-1.5 px-2.5 h-10 rounded-md flex items-center gap-3 cursor-pointer text-[13px]"
            :class="i === sel ? 'bg-ms-well text-ms-text' : 'text-ms-text2'"
            @mouseenter="sel = i" @click="pick(r)">
            <Icon :name="r.icon" :size="16" class="text-ms-muted" />
            <span class="truncate" :class="r.mono && r.label === r.hint ? 'ms-id' : ''">{{ r.label }}</span>
            <span v-if="r.hint && r.hint !== r.label" class="truncate text-xs text-ms-muted" :class="r.mono ? 'ms-id' : ''">{{ r.hint }}</span>
            <span class="ml-auto text-xs text-ms-muted">{{ r.kind }}</span>
          </li>
        </ul>
      </div>
    </div>
  </Teleport>
</template>
