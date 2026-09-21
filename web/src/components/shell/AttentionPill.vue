<script setup>
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useRouter } from 'vue-router'
import { useOpsStore } from '../../stores/ops'
import { useAuthStore } from '../../stores/auth'
import { useToastStore } from '../../stores/toast'
import { timeAgo } from '../../utils/time'
import Icon from '../Icon.vue'

// The alarm summary every page carries (ISA-18.2): what is unacknowledged,
// highest priority first, one click to acknowledge or to the source. When
// nothing needs a person it says so quietly and takes no colour at all.
const ops = useOpsStore()
const auth = useAuthStore()
const toast = useToastStore()
const router = useRouter()
const open = ref(false)
const busy = ref('')
const root = ref(null)

const items = computed(() => ops.attention)
const count = computed(() => items.value.length)
const sosCount = computed(() => items.value.filter((i) => i.kind === 'sos').length)
const canAck = computed(() => auth.role !== 'viewer')

const label = computed(() => {
  if (!ops.loaded) return 'Checking'
  if (sosCount.value) return sosCount.value === 1 ? 'SOS' : `${sosCount.value} SOS`
  if (!count.value) return 'All clear'
  return count.value === 1 ? '1 needs you' : `${count.value} need you`
})

async function ack(item) {
  busy.value = item.key
  try {
    await ops.acknowledge(item.alertId)
    toast.success('Acknowledged')
  } catch (e) {
    toast.error(`Acknowledge failed: ${e.message}`)
  } finally {
    busy.value = ''
  }
}

function go(item) {
  open.value = false
  router.push(item.to)
}

function onDoc(e) {
  if (open.value && root.value && !root.value.contains(e.target)) open.value = false
}
function onKey(e) { if (e.key === 'Escape') open.value = false }
onMounted(() => { document.addEventListener('mousedown', onDoc); document.addEventListener('keydown', onKey) })
onUnmounted(() => { document.removeEventListener('mousedown', onDoc); document.removeEventListener('keydown', onKey) })
</script>

<template>
  <div ref="root" class="relative">
    <button type="button" data-testid="attention"
      class="inline-flex items-center gap-2 h-8 pl-2.5 pr-3 rounded-full text-[13px] font-medium border transition-colors whitespace-nowrap"
      :class="ops.worst === 'sos' ? 'bg-ms-error text-ms-on-primary border-ms-error'
        : ops.worst === 'alarm' ? 'border-ms-error text-ms-error hover:bg-ms-error/10'
        : ops.worst === 'caution' ? 'border-ms-warning/70 text-ms-warning hover:bg-ms-warning/10'
        : 'border-ms-border text-ms-muted hover:text-ms-text hover:bg-ms-well'"
      :aria-expanded="open" aria-haspopup="dialog" @click="open = !open">
      <Icon v-if="ops.worst === 'sos' || ops.worst === 'alarm'" name="alerts" :size="15" />
      <span v-else-if="ops.worst === 'caution'" class="w-2.5 h-2.5 rounded-full border-2 border-current" aria-hidden="true" />
      <span v-else class="w-2 h-2 rounded-full border border-current opacity-80" aria-hidden="true" />
      <span class="ms-num" :class="!ops.worst ? 'hidden sm:inline' : ''">{{ label }}</span><span v-if="!ops.worst" class="sr-only sm:hidden">{{ label }}</span>
    </button>

    <div v-if="open" role="dialog" aria-label="Needs attention"
      class="absolute right-0 mt-2 w-[min(380px,calc(100vw-24px))] ms-panel shadow-2xl shadow-black/40 z-50 overflow-hidden">
      <div class="ms-panel-head">
        <span class="ms-h2">Needs attention</span>
        <span class="text-xs text-ms-muted" v-if="ops.loadedAt">Checked {{ timeAgo(ops.loadedAt) }}</span>
      </div>
      <div v-if="!count" class="px-4 py-6 text-sm text-ms-muted">
        Nothing needs you. Unacknowledged alerts, missed check-ins and kits that drop off appear here.
      </div>
      <ul v-else class="max-h-[60vh] overflow-y-auto tactical-scroll divide-y divide-ms-border">
        <li v-for="item in items" :key="item.key" class="px-4 py-3 flex gap-3">
          <span class="mt-0.5" :class="item.kind === 'caution' ? 'text-ms-warning' : 'text-ms-error'">
            <Icon v-if="item.kind !== 'caution'" name="alerts" :size="16" />
            <span v-else class="block w-3 h-3 mt-0.5 rounded-full border-2 border-current" aria-hidden="true" />
          </span>
          <div class="min-w-0 flex-1">
            <div class="flex items-baseline gap-2">
              <span class="text-[13px] font-semibold" :class="item.kind === 'caution' ? 'text-ms-text' : 'text-ms-error'">{{ item.title }}</span>
              <span class="ms-id text-ms-text2 truncate">{{ item.subject }}</span>
              <span class="ml-auto text-xs text-ms-muted whitespace-nowrap">{{ timeAgo(item.since) }}</span>
            </div>
            <p v-if="item.detail" class="text-xs text-ms-muted mt-0.5 line-clamp-2">{{ item.detail }}</p>
            <div class="flex gap-2 mt-2">
              <button v-if="item.alertId && canAck" class="ms-btn-primary h-7 px-2.5 text-xs" :disabled="busy === item.key" @click="ack(item)">
                {{ busy === item.key ? 'Acknowledging' : 'Acknowledge' }}
              </button>
              <button class="ms-btn h-7 px-2.5 text-xs" @click="go(item)">Open</button>
            </div>
          </div>
        </li>
      </ul>
    </div>
  </div>
</template>
