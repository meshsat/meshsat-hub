<script setup>
import { ref, computed, watch, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { messages as messagesApi, credits as creditsApi } from '../api/client'
import { useOpsStore } from '../stores/ops'
import { useAuthStore } from '../stores/auth'
import { useToastStore } from '../stores/toast'
import { FAMILIES, familyStates, bearerOf, bodyOf, ago, hhmm } from '../utils/paths'
import Icon from '../components/Icon.vue'
import PathCell from '../components/PathCell.vue'

// Overview: in two seconds, does anyone need me, and what can each kit use
// right now. Built on the shell's shared poll (stores/ops.js); only the
// message stream is fetched here, and it refreshes whenever the poll does.
const ops = useOpsStore()
const auth = useAuthStore()
const toast = useToastStore()
const router = useRouter()

const recent = ref([])
const msgLoaded = ref(false)
const busy = ref('')

async function loadMessages() {
  try {
    const m = await messagesApi.list('', 200)
    if (Array.isArray(m)) recent.value = m
  } catch { /* the stream is informational; the poll retries */ }
  msgLoaded.value = true
}
// The Iridium credit balance of this account's own Cloudloop account. Most
// accounts bring none and /api/credits answers 404 for good: stop asking
// then (MESHSAT-1111), and show nothing rather than a zero. The figure is
// stated plainly; a balance is not an alarm.
const credit = ref(null)
let creditsUnavailable = false
async function loadCredits() {
  if (creditsUnavailable) return
  try {
    const c = await creditsApi.get()
    credit.value = typeof c?.balance === 'number' ? c.balance : null
  } catch (e) {
    if (e?.status === 404) creditsUnavailable = true
  }
}

onMounted(() => { loadMessages(); loadCredits() })
watch(() => ops.loadedAt, () => { loadMessages(); loadCredits() })

const kits = computed(() => [...ops.kits].sort((a, b) => Number(b.online) - Number(a.online) || a.name.localeCompare(b.name)))
const onlineKits = computed(() => kits.value.filter((k) => k.online).length)
const deviceName = computed(() => Object.fromEntries(ops.devices.map((d) => [d.imei, d.label || ''])))

const rows = computed(() => kits.value.map((k) => {
  const states = familyStates(k.interfaces)
  const fitted = FAMILIES.filter((f) => states[f.key] !== 'absent').length
  const working = FAMILIES.filter((f) => states[f.key] === 'up').length
  return { ...k, states, fitted, working }
}))

// The one-line summary under the title. Plain words, facts only.
const summary = computed(() => {
  if (!ops.loaded) return 'Loading the fleet.'
  const parts = []
  if (kits.value.length) {
    parts.push(onlineKits.value === kits.value.length
      ? `${kits.value.length === 1 ? 'Your kit is' : `All ${kits.value.length} kits are`} connected.`
      : `${onlineKits.value} of ${kits.value.length} kits connected.`)
    // A kit with radios fitted and none working reaches the Hub over its own
    // internet link only; one working radio is a kit with no redundancy left.
    const none = rows.value.filter((r) => r.online && r.fitted > 0 && r.working === 0)
    const one = rows.value.filter((r) => r.online && r.fitted > 1 && r.working === 1)
    if (none.length === 1) parts.push(`${none[0].name} is connected, but none of its radios is working.`)
    else if (none.length > 1) parts.push(`${none.length} kits are connected with no radio working.`)
    if (one.length === 1) parts.push(`${one[0].name} is down to one radio.`)
    else if (one.length > 1) parts.push(`${one.length} kits are down to one radio.`)
  }
  if (recent.value.length) {
    const m = recent.value[0]
    parts.push(`Last message ${ago(m.created_at)}, over ${bearerOf(m).label.toLowerCase()}.`)
  }
  return parts.join(' ') || 'No kits or devices yet.'
})

const stream = computed(() => recent.value.slice(0, 9).map((m) => ({
  ...m,
  bearer: bearerOf(m),
  body: bodyOf(m),
  who: deviceName.value[m.device_imei] || '',
})))

// 24 hourly buckets, oldest first, for the traffic strip.
const hours = computed(() => {
  const now = Date.now()
  const b = new Array(24).fill(0)
  for (const m of recent.value) {
    const h = Math.floor((now - new Date(m.created_at).getTime()) / 3600000)
    if (h >= 0 && h < 24) b[23 - h]++
  }
  return b
})
const dayTotal = computed(() => hours.value.reduce((a, b) => a + b, 0))
const peak = computed(() => Math.max(1, ...hours.value))

const checkinFor = computed(() => Object.fromEntries(ops.checkins.filter((c) => c.enabled).map((c) => [c.device_imei, c])))
const devices = computed(() => [...ops.devices]
  .sort((a, b) => new Date(b.last_seen || 0) - new Date(a.last_seen || 0))
  .slice(0, 8)
  .map((d) => ({ ...d, checkin: checkinFor.value[d.imei] })))

const TYPE_WORDS = { iridium_imt: 'Iridium 9704', iridium_sbd: 'Iridium SBD', rockblock: 'RockBLOCK', globalstar: 'Globalstar', meshtastic: 'Mesh node' }

const firstRun = computed(() => ops.loaded && !ops.kits.length && !ops.devices.length)
const canAct = computed(() => auth.role !== 'viewer')

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

function openKit(k) { router.push({ name: 'fleet', query: { kit: k.id } }) }
</script>

<template>
  <div class="ms-page">
    <div class="ms-page-head">
      <div>
        <h1 class="ms-h1">Overview</h1>
        <p class="ms-lede" data-testid="summary">{{ summary }}</p>
      </div>
      <button class="ms-btn-ghost text-xs" :title="'Refresh now'" @click="ops.load(); loadMessages()">
        <Icon name="refresh" :size="15" />
        <span v-if="ops.loadedAt">Updated {{ ago(ops.loadedAt) }}</span>
      </button>
    </div>

    <!-- Attention: loud only when something needs a person -->
    <section v-if="ops.attention.length" aria-labelledby="attn-h" class="mb-6 rounded-[10px] border overflow-hidden"
      :class="ops.worst === 'caution' ? 'border-ms-warning/50' : 'border-ms-error/70'">
      <div class="flex items-center gap-2 px-4 h-11 border-b"
        :class="ops.worst === 'sos' ? 'bg-ms-error text-ms-on-primary border-ms-error' : ops.worst === 'alarm' ? 'bg-ms-error/10 text-ms-error border-ms-error/40' : 'bg-ms-warning/10 text-ms-warning border-ms-warning/40'">
        <Icon name="alerts" :size="16" />
        <h2 id="attn-h" class="text-sm font-semibold">
          {{ ops.attention.length === 1 ? 'One thing needs you' : `${ops.attention.length} things need you` }}
        </h2>
      </div>
      <ul class="divide-y divide-ms-border bg-ms-card">
        <li v-for="item in ops.attention" :key="item.key" class="flex flex-wrap items-center gap-x-4 gap-y-2 px-4 py-3">
          <span class="w-28 shrink-0 text-[13px] font-semibold" :class="item.kind === 'caution' ? 'text-ms-warning' : 'text-ms-error'">{{ item.title }}</span>
          <span class="ms-id text-ms-text">{{ item.subject }}</span>
          <span class="text-[13px] text-ms-muted flex-1 min-w-[12rem] truncate">{{ item.detail }}</span>
          <span v-if="item.since" class="text-xs text-ms-muted whitespace-nowrap">{{ ago(item.since) }}</span>
          <div class="flex gap-2">
            <button v-if="item.alertId && canAct" class="ms-btn-primary" :disabled="busy === item.key" @click="ack(item)">
              {{ busy === item.key ? 'Acknowledging' : 'Acknowledge' }}
            </button>
            <router-link :to="item.to" class="ms-btn">Open</router-link>
          </div>
        </li>
      </ul>
    </section>
    <p v-else-if="ops.loaded" class="mb-6 flex items-center gap-2 text-[13px] text-ms-muted" data-testid="all-clear">
      <span class="w-2 h-2 rounded-full border border-ms-muted" aria-hidden="true" />
      Nothing needs you right now.
    </p>

    <!-- First run: a real sequence, so it is numbered -->
    <section v-if="firstRun" class="ms-panel p-5 mb-6" data-testid="first-run">
      <h2 class="ms-h2">Set up your first kit</h2>
      <p class="text-[13px] text-ms-muted mt-1 max-w-[62ch]">Three steps and the Hub starts relaying your messages. Each kit brings its own satellite and SMS accounts, so nothing is sent on anyone else's airtime.</p>
      <ol class="mt-4 grid gap-3 sm:grid-cols-3">
        <li class="rounded-lg border border-ms-border p-4">
          <div class="text-xs text-ms-muted">Step 1</div>
          <div class="text-sm font-medium mt-0.5">Add a kit</div>
          <p class="text-xs text-ms-muted mt-1">Scan a QR code on the kit or the Android app and it connects itself.</p>
          <router-link :to="{ name: 'fleet', query: { add: '1' } }" class="ms-btn-primary mt-3">Add your first kit</router-link>
        </li>
        <li class="rounded-lg border border-ms-border p-4">
          <div class="text-xs text-ms-muted">Step 2</div>
          <div class="text-sm font-medium mt-0.5">Connect your providers</div>
          <p class="text-xs text-ms-muted mt-1">Your Cloudloop, Rock7 or Twilio account, so messages leave on your airtime.</p>
          <router-link to="/settings" class="ms-btn mt-3">Open settings</router-link>
        </li>
        <li class="rounded-lg border border-ms-border p-4">
          <div class="text-xs text-ms-muted">Step 3</div>
          <div class="text-sm font-medium mt-0.5">Decide who hears an SOS</div>
          <p class="text-xs text-ms-muted mt-1">An escalation chain notifies people in order until someone acknowledges.</p>
          <router-link to="/escalation" class="ms-btn mt-3">Set up alerts</router-link>
        </li>
      </ol>
    </section>

    <div class="grid gap-5 xl:grid-cols-12 items-start">
      <div class="xl:col-span-7 space-y-5 min-w-0">
      <!-- Paths: the signature view -->
      <section class="ms-panel overflow-hidden" aria-labelledby="paths-h">
        <div class="ms-panel-head">
          <div class="flex items-baseline gap-3 min-w-0">
            <h2 id="paths-h" class="ms-h2">Paths</h2>
            <span class="text-xs text-ms-muted truncate hidden sm:inline">What each kit can use right now</span>
          </div>
          <router-link to="/fleet" class="ms-btn-ghost text-xs">All kits</router-link>
        </div>
        <div v-if="!ops.loaded" class="px-4 py-8 text-sm text-ms-muted">Loading kits.</div>
        <div v-else-if="!rows.length" class="px-4 py-8 text-sm text-ms-muted">
          No kits yet. <router-link :to="{ name: 'fleet', query: { add: '1' } }" class="text-ms-primary hover:underline">Add one</router-link> and its paths show up here.
        </div>
        <div v-else class="overflow-x-auto">
          <table class="ms-table" data-testid="paths">
            <thead>
              <tr>
                <th>Kit</th>
                <th v-for="f in FAMILIES" :key="f.key" class="!px-1 sm:!px-2 text-center" :title="f.long">
                  <span class="hidden sm:inline">{{ f.label }}</span><span class="sm:hidden">{{ f.label.slice(0, 1) }}</span>
                </th>
                <th class="text-right hidden md:table-cell">Hub link</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="r in rows" :key="r.id" class="cursor-pointer hover:bg-ms-well/50" tabindex="0"
                @click="openKit(r)" @keydown.enter="openKit(r)">
                <td class="!h-14 max-w-0 w-full">
                  <div class="flex items-center gap-2.5 min-w-0">
                    <span class="w-2 h-2 rounded-full shrink-0" :class="r.online ? 'bg-ms-success' : 'border border-ms-warning'" :title="r.online ? 'Connected' : 'Offline'" />
                    <div class="min-w-0">
                      <div class="text-[13px] font-medium text-ms-text truncate">{{ r.name }}</div>
                      <div class="ms-id text-ms-muted truncate">{{ r.id }}</div>
                    </div>
                  </div>
                </td>
                <td v-for="f in FAMILIES" :key="f.key" class="!px-0 sm:!px-2 text-center">
                  <PathCell :state="r.online ? r.states[f.key] : (r.states[f.key] === 'absent' ? 'absent' : 'down')" :label="f.long" />
                </td>
                <td class="text-right whitespace-nowrap hidden md:table-cell">
                  <span v-if="r.online" class="text-[13px] text-ms-text2">Live</span>
                  <span v-else class="text-[13px] text-ms-warning">Offline, {{ ago(r.lastSeen) }}</span>
                </td>
              </tr>
            </tbody>
          </table>
          <div class="flex flex-wrap items-center gap-x-5 gap-y-1 px-4 py-2.5 border-t border-ms-border text-xs text-ms-muted">
            <span class="inline-flex items-center gap-1.5"><PathCell state="up" label="Example" /> working</span>
            <span class="inline-flex items-center gap-1.5"><PathCell state="coming" label="Example" /> coming up</span>
            <span class="inline-flex items-center gap-1.5"><PathCell state="down" label="Example" /> fitted, not working</span>
            <span class="inline-flex items-center gap-1.5"><PathCell state="absent" label="Example" /> not fitted</span>
          </div>
        </div>
      </section>

      <!-- Devices -->
      <section class="ms-panel overflow-hidden" aria-labelledby="dev-h">
        <div class="ms-panel-head">
          <div class="flex items-baseline gap-3 min-w-0">
            <h2 id="dev-h" class="ms-h2">Devices</h2>
            <span class="text-xs text-ms-muted truncate hidden sm:inline">Quiet is normal unless a check-in is due</span>
          </div>
          <router-link to="/devices" class="ms-btn-ghost text-xs">All devices</router-link>
        </div>
        <div v-if="ops.loaded && !devices.length" class="px-4 py-8 text-sm text-ms-muted">
          No satellite devices registered. <router-link to="/devices" class="text-ms-primary hover:underline">Register one</router-link> to track its messages and position.
        </div>
        <table v-else class="ms-table">
          <tbody>
            <tr v-for="d in devices" :key="d.imei" class="cursor-pointer hover:bg-ms-well/50" tabindex="0"
              @click="router.push({ name: 'deviceDetail', params: { imei: d.imei } })"
              @keydown.enter="router.push({ name: 'deviceDetail', params: { imei: d.imei } })">
              <td class="w-full">
                <div class="text-[13px] font-medium text-ms-text truncate">{{ d.label || d.imei }}</div>
                <div class="ms-id text-ms-muted">{{ d.imei }}</div>
              </td>
              <td class="whitespace-nowrap text-[13px] text-ms-muted hidden sm:table-cell">{{ TYPE_WORDS[d.type] || d.type }}</td>
              <td class="whitespace-nowrap text-right">
                <div v-if="d.checkin && d.checkin.alerted" class="text-[13px] font-medium text-ms-error">Missed check-in</div>
                <div v-else class="text-[13px] text-ms-text2">Heard {{ ago(d.last_seen) }}</div>
                <div v-if="d.checkin" class="text-xs text-ms-muted">Check-in every {{ Math.round(d.checkin.interval_sec / 60) }} min</div>
              </td>
            </tr>
          </tbody>
        </table>
      </section>

      </div>

      <div class="xl:col-span-5 space-y-5 min-w-0">
      <!-- Traffic -->
      <section class="ms-panel overflow-hidden flex flex-col" aria-labelledby="traffic-h">
        <div class="ms-panel-head">
          <h2 id="traffic-h" class="ms-h2">Traffic</h2>
          <router-link to="/messages" class="ms-btn-ghost text-xs">All messages</router-link>
        </div>
        <div class="px-4 pt-3 pb-2 border-b border-ms-border">
          <div class="flex items-end gap-[3px] h-9" role="img" :aria-label="`${dayTotal} messages in the last 24 hours`">
            <span v-for="(n, i) in hours" :key="i" class="flex-1 rounded-[1px]"
              :class="n ? 'bg-ms-text2/70' : 'bg-ms-border'"
              :style="{ height: n ? Math.max(12, (n / peak) * 100) + '%' : '2px' }"
              :title="`${n} message${n === 1 ? '' : 's'}, ${23 - i === 0 ? 'this hour' : (23 - i) + ' h ago'}`" />
          </div>
          <div class="flex justify-between mt-1.5 text-xs text-ms-muted">
            <span>24 h ago</span>
            <span class="ms-num">{{ dayTotal }} in the last 24 hours</span>
            <span>now</span>
          </div>
          <div v-if="credit !== null" class="mt-2 text-xs text-ms-muted" data-testid="credits">
            Iridium credit balance <span class="ms-num text-ms-text2">{{ credit.toLocaleString() }}</span>
          </div>
        </div>
        <div v-if="msgLoaded && !stream.length" class="px-4 py-8 text-sm text-ms-muted">No messages yet. They appear here the moment a kit or device sends one.</div>
        <ul v-else class="divide-y divide-ms-border/70 flex-1">
          <li v-for="m in stream" :key="m.id" class="px-4 py-2.5">
            <div class="flex items-baseline gap-2 min-w-0">
              <span class="font-mono text-xs text-ms-muted ms-num">{{ hhmm(m.created_at) }}</span>
              <span class="text-xs" :class="m.direction === 'mt' ? 'text-ms-primary' : 'text-ms-muted'">{{ m.direction === 'mt' ? 'Out' : 'In' }}</span>
              <span v-if="m.who" class="text-[13px] font-medium text-ms-text truncate">{{ m.who }}</span>
              <span v-else class="ms-id text-ms-text2 truncate">{{ m.device_imei }}</span>
              <span class="ml-auto text-xs text-ms-muted shrink-0">{{ m.bearer.label }}</span>
            </div>
            <p class="text-[13px] mt-0.5 truncate pl-[3.25rem]" :class="m.body.kind === 'text' ? 'text-ms-text2' : 'text-ms-muted italic'">
              <Icon v-if="m.body.kind === 'sealed'" name="lock" :size="12" class="inline -mt-0.5 mr-1" />{{ m.body.text }}
            </p>
          </li>
        </ul>
      </section>

      <!-- Shortcuts to the jobs people come here for -->
      <section class="grid grid-cols-2 gap-3" aria-label="Shortcuts">
        <router-link v-for="s in [
            { to: '/map', icon: 'map', t: 'Map', d: 'Where every kit and device last reported' },
            { to: '/messages', icon: 'messages', t: 'Send a message', d: 'To a satellite device or a phone' },
            { to: '/routing', icon: 'routing', t: 'Routing', d: 'Which messages go where' },
            { to: '/escalation', icon: 'alerts', t: 'Alerts', d: 'Who is called for an SOS, and in what order' },
          ]" :key="s.to" :to="s.to"
          class="group rounded-[10px] border border-ms-border p-4 hover:border-ms-border-light hover:bg-ms-card transition-colors">
          <Icon :name="s.icon" :size="18" class="text-ms-muted group-hover:text-ms-primary transition-colors" />
          <div class="text-[13px] font-medium mt-3">{{ s.t }}</div>
          <div class="text-xs text-ms-muted mt-0.5">{{ s.d }}</div>
        </router-link>
      </section>
      </div>
    </div>
  </div>
</template>
