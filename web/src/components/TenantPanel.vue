<script setup>
// Tenant panel (Settings): name and invites for the signed-in tenant.
// Owner-only. Talks to /api/tenant (MR 16); stays hidden while that API
// is absent so the panel can ship ahead of it.
import { ref, computed, onMounted } from 'vue'
import { tenant as tenantApi } from '../api/client'
import { useAuthStore } from '../stores/auth'
import { useToastStore } from '../stores/toast'

const auth = useAuthStore()
const toast = useToastStore()

const available = ref(false)
const info = ref(null)
const name = ref('')
// Bridge offline timeout (MESHSAT-1117). '' means the tenant has chosen
// nothing and the platform default applies; the API sends that default back so
// this can show the number actually in force rather than hardcoding one.
const bridgeTimeout = ref('')
const savingTimeout = ref(false)
// Audit retention (MESHSAT-1117 tranche 2b), same shape as the timeout above.
const auditDays = ref('')
const savingAudit = ref(false)
// Send budget: how many messages the Hub sends to one device per day / month.
const capDaily = ref('')
const capMonthly = ref('')
const savingCaps = ref(false)
const savingOOB = ref(false)
const oobMax = ref('')
const oobSms = ref('')
const oobSat = ref('')
const saving = ref(false)
const invites = ref([])
const inviteEmail = ref('')
const inviteRole = ref('operator')
const inviting = ref(false)
const error = ref('')
// Offboarding (Phase 6). The API has existed since the launch; until now there
// was no way to reach it without an API key, which is not a thing you can ask a
// customer for when they want to leave.
const exporting = ref(false)
const closing = ref(false)
const confirmClose = ref(false)
const confirmText = ref('')
const usage = ref(null)

// Devices and bridges share one ceiling, because that is the number somebody
// can count against their own kit. -1 means the plan has no ceiling.
const unlimited = computed(() => usage.value?.limit === -1)
const usageBadge = computed(() => {
  if (!usage.value || unlimited.value) return 'bg-sky-900/50 text-sky-300 border-sky-700/50'
  if (usage.value.over_limit) return 'bg-amber-900/50 text-amber-300 border-amber-700/50'
  if (usage.value.remaining === 0) return 'bg-amber-900/50 text-amber-300 border-amber-700/50'
  return 'bg-emerald-900/50 text-emerald-300 border-emerald-700/50'
})

// Checkout starts here rather than at an external link, so the session carries
// this tenant and the payment can never arrive unattributed.
const billingInHub = computed(() => usage.value?.billing?.provider === 'stripe')
const checkoutBusy = ref(false)

// Only the tiers that can actually be bought. custom and beta are operator-set
// and unlimited, and the server refuses to sell them; offering them here would
// produce a button that always fails.
// Plan names are stored lowercase ('crew', 'fleet') but they are proper names,
// so they are capitalised for display. The button used to carry a Tailwind
// `capitalize` class instead, which title-cased the whole label and rendered
// "Subscribe To Crew" (MESHSAT-1252). House style is sentence case.
function planName(plan) {
  if (!plan) return ''
  return plan.charAt(0).toUpperCase() + plan.slice(1)
}

const buyable = computed(() =>
  (usage.value?.tiers || []).filter((t) => t.plan === 'crew' || t.plan === 'fleet'))

async function subscribe(plan) {
  if (checkoutBusy.value) return
  checkoutBusy.value = true
  try {
    const { url } = await tenantApi.checkout(plan)
    // Same tab: a popup blocker eating a payment page looks like a broken button.
    window.location.href = url
  } catch (e) {
    toast.error(e?.message || 'Could not start checkout. Please try again.')
    checkoutBusy.value = false
  }
}

async function donate() {
  if (checkoutBusy.value) return
  checkoutBusy.value = true
  try {
    const { url } = await tenantApi.donate()
    window.location.href = url
  } catch (e) {
    toast.error(e?.message || 'Could not start the donation. Please try again.')
    checkoutBusy.value = false
  }
}

async function manageBilling() {
  if (checkoutBusy.value) return
  checkoutBusy.value = true
  try {
    const { url } = await tenantApi.billingPortal()
    window.location.href = url
  } catch (e) {
    toast.error(e?.message || 'Could not open the billing portal.')
    checkoutBusy.value = false
  }
}

async function load() {
  try {
    info.value = await tenantApi.get()
    name.value = info.value.name || ''
    bridgeTimeout.value = info.value.bridge_offline_timeout ? String(info.value.bridge_offline_timeout) : ''
    auditDays.value = info.value.audit_retention_days ? String(info.value.audit_retention_days) : ''
    syncCaps()
    oobMax.value = info.value.oob_max_per_hour ? String(info.value.oob_max_per_hour) : ''
    oobSms.value = info.value.oob_sms_timeout_sec ? String(info.value.oob_sms_timeout_sec) : ''
    oobSat.value = info.value.oob_sat_timeout_sec ? String(info.value.oob_sat_timeout_sec) : ''
    available.value = true
  } catch {
    available.value = false
    return
  }
  try {
    usage.value = await tenantApi.usage()
  } catch {
    usage.value = null
  }
  try {
    invites.value = (await tenantApi.invites()) || []
  } catch {
    invites.value = []
  }
}

async function saveName() {
  if (!name.value.trim()) return
  saving.value = true
  error.value = ''
  try {
    info.value = await tenantApi.update({ name: name.value.trim() })
    toast.success('Tenant name saved')
  } catch (e) {
    error.value = e.message || 'Save failed'
  } finally {
    saving.value = false
  }
}

async function saveBridgeTimeout() {
  const raw = bridgeTimeout.value.trim()
  // Empty means "use the platform default", which the API takes as 0.
  const secs = raw === '' ? 0 : Number(raw)
  if (!Number.isInteger(secs) || secs < 0) {
    error.value = 'Offline timeout must be a whole number of seconds'
    return
  }
  savingTimeout.value = true
  error.value = ''
  try {
    info.value = await tenantApi.update({ name: name.value.trim(), bridge_offline_timeout: secs })
    bridgeTimeout.value = info.value.bridge_offline_timeout ? String(info.value.bridge_offline_timeout) : ''
    toast.success(secs === 0 ? 'Using the platform default' : 'Offline timeout saved')
  } catch (e) {
    error.value = e.message || 'Save failed'
  } finally {
    savingTimeout.value = false
  }
}

function syncCaps() {
  capDaily.value = info.value?.ratelimit_daily_cap ? String(info.value.ratelimit_daily_cap) : ''
  capMonthly.value = info.value?.ratelimit_monthly_cap ? String(info.value.ratelimit_monthly_cap) : ''
}

const capsUnchanged = () =>
  capDaily.value.trim() === (info.value?.ratelimit_daily_cap ? String(info.value.ratelimit_daily_cap) : '') &&
  capMonthly.value.trim() === (info.value?.ratelimit_monthly_cap ? String(info.value.ratelimit_monthly_cap) : '')

// The send budget (owner ruling, 21 Sep 2026). The airtime is this tenant's
// own carrier account, so the limit on it is this tenant's to set. One save
// for the pair: they are one decision.
async function saveSendCaps() {
  const daily = capDaily.value.trim() === '' ? 0 : Number(capDaily.value.trim())
  const monthly = capMonthly.value.trim() === '' ? 0 : Number(capMonthly.value.trim())
  if (![daily, monthly].every(n => Number.isInteger(n) && n >= 0)) {
    error.value = 'A send limit must be a whole number of messages'
    return
  }
  savingCaps.value = true
  error.value = ''
  try {
    info.value = await tenantApi.update({
      name: name.value.trim(), ratelimit_daily_cap: daily, ratelimit_monthly_cap: monthly,
    })
    syncCaps()
    toast.success(daily === 0 && monthly === 0 ? 'Using the platform defaults' : 'Send limits saved')
  } catch (e) {
    error.value = e.message || 'Save failed'
  } finally {
    savingCaps.value = false
  }
}

async function saveAuditRetention() {
  const raw = auditDays.value.trim()
  const days = raw === '' ? 0 : Number(raw)
  if (!Number.isInteger(days) || days < 0) {
    error.value = 'Audit retention must be a whole number of days'
    return
  }
  savingAudit.value = true
  error.value = ''
  try {
    info.value = await tenantApi.update({ name: name.value.trim(), audit_retention_days: days })
    auditDays.value = info.value.audit_retention_days ? String(info.value.audit_retention_days) : ''
    toast.success(days === 0 ? 'Using the platform default' : 'Audit retention saved')
  } catch (e) {
    error.value = e.message || 'Save failed'
  } finally {
    savingAudit.value = false
  }
}

// Out-of-band command policy (MESHSAT-1121). One save for the three, because
// they are one decision: how hard this tenant drives its own field kit.
async function saveOOB() {
  const parse = (raw, label) => {
    const t = raw.trim()
    if (t === '') return 0
    const n = Number(t)
    if (!Number.isInteger(n) || n < 0) throw new Error(`${label} must be a whole number`)
    return n
  }
  savingOOB.value = true
  error.value = ''
  try {
    info.value = await tenantApi.update({
      name: name.value.trim(),
      oob_max_per_hour: parse(oobMax.value, 'Commands per hour'),
      oob_sms_timeout_sec: parse(oobSms.value, 'SMS reply timeout'),
      oob_sat_timeout_sec: parse(oobSat.value, 'Satellite reply timeout'),
    })
    oobMax.value = info.value.oob_max_per_hour ? String(info.value.oob_max_per_hour) : ''
    oobSms.value = info.value.oob_sms_timeout_sec ? String(info.value.oob_sms_timeout_sec) : ''
    oobSat.value = info.value.oob_sat_timeout_sec ? String(info.value.oob_sat_timeout_sec) : ''
    toast.success('Command policy saved')
  } catch (e) {
    error.value = e.message || 'Save failed'
  } finally {
    savingOOB.value = false
  }
}

async function sendInvite() {
  const email = inviteEmail.value.trim().toLowerCase()
  if (!email) return
  inviting.value = true
  error.value = ''
  try {
    await tenantApi.invite({ email, role: inviteRole.value })
    inviteEmail.value = ''
    toast.success(`Invite created for ${email}`)
    invites.value = (await tenantApi.invites()) || []
  } catch (e) {
    error.value = e.message || 'Invite failed'
  } finally {
    inviting.value = false
  }
}

async function revoke(inv) {
  try {
    await tenantApi.revokeInvite(inv.id)
    invites.value = invites.value.filter((i) => i.id !== inv.id)
  } catch (e) {
    error.value = e.message || 'Revoke failed'
  }
}

// A customer must be able to take their data with them and to leave. Both are
// rights under GDPR (portability, erasure) and both were API-only until now.
async function exportData() {
  exporting.value = true
  error.value = ''
  try {
    const blob = await tenantApi.exportData()
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `meshsat-hub-${info.value?.slug || 'tenant'}-${new Date().toISOString().slice(0, 10)}.zip`
    document.body.appendChild(a)
    a.click()
    a.remove()
    URL.revokeObjectURL(url)
    toast.success('Export downloaded')
  } catch (e) {
    error.value = e.message
  } finally {
    exporting.value = false
  }
}

async function closeAccount() {
  closing.value = true
  error.value = ''
  try {
    await tenantApi.close()
    toast.success('Account closed. You can still change your mind during the grace period.')
    confirmClose.value = false
    await load()
  } catch (e) {
    error.value = e.message
  } finally {
    closing.value = false
  }
}

function fmt(ts) {
  return ts ? new Date(ts).toLocaleDateString() : ''
}

onMounted(load)
</script>

<template>
  <div v-if="available && auth.isOwner" class="bg-tactical-surface rounded-lg border border-tactical-border p-4" data-testid="tenant-panel">
    <h2 class="text-sm font-display font-semibold text-ms-text uppercase tracking-wider mb-1">Tenant</h2>
    <p class="text-xs text-ms-muted mb-4">
      Your organisation on this Hub. Invited people sign in with their MeshSat ID and land here with the role you choose.
    </p>

    <div class="grid gap-4 md:grid-cols-2">
      <div>
        <label for="tenant-name" class="block text-xs text-ms-muted2 mb-1">Name</label>
        <div class="flex gap-2">
          <input id="tenant-name" v-model="name" type="text" maxlength="80"
            class="flex-1 min-w-0 px-3 py-1.5 bg-ms-well border border-ms-border rounded text-sm text-ms-text focus:outline-none focus:border-brand-primary" />
          <button @click="saveName" :disabled="saving || !name.trim() || name.trim() === info?.name"
            class="px-3 py-1.5 bg-brand-primary hover:bg-brand-accent disabled:opacity-50 text-ms-on-primary text-sm font-medium rounded transition-colors">
            Save
          </button>
        </div>

        <!-- Bridge offline timeout (MESHSAT-1117): a fleet setting that used to
             be one environment variable for every tenant on the platform. -->
        <label for="tenant-bridge-timeout" class="block text-xs text-ms-muted2 mt-4 mb-1">
          Mark a bridge offline after
        </label>
        <div class="flex gap-2 items-center">
          <input id="tenant-bridge-timeout" v-model="bridgeTimeout" type="number" inputmode="numeric"
            :min="info?.bridge_offline_timeout_min" :max="info?.bridge_offline_timeout_max"
            :placeholder="String(info?.bridge_offline_timeout_default ?? '')"
            class="w-28 min-w-0 px-3 py-1.5 bg-ms-well border border-ms-border rounded text-sm text-ms-text focus:outline-none focus:border-brand-primary" />
          <span class="text-xs text-ms-muted">seconds of silence</span>
          <button @click="saveBridgeTimeout"
            :disabled="savingTimeout || bridgeTimeout.trim() === (info?.bridge_offline_timeout ? String(info.bridge_offline_timeout) : '')"
            class="px-3 py-1.5 bg-brand-primary hover:bg-brand-accent disabled:opacity-50 text-ms-on-primary text-sm font-medium rounded transition-colors">
            Save
          </button>
        </div>
        <p class="mt-1 text-xs text-ms-muted">
          Leave empty for the platform default ({{ info?.bridge_offline_timeout_default }}s).
          Between {{ info?.bridge_offline_timeout_min }} and {{ info?.bridge_offline_timeout_max }} seconds.
          This only changes when a bridge is shown as offline; it never affects SOS or message delivery.
        </p>

        <!-- Audit retention (MESHSAT-1117). The audit log is this tenant's own
             record of who did what in its account. -->
        <label for="tenant-audit-days" class="block text-xs text-ms-muted2 mt-4 mb-1">
          Keep audit log entries for
        </label>
        <div class="flex gap-2 items-center">
          <input id="tenant-audit-days" v-model="auditDays" type="number" inputmode="numeric"
            :min="info?.audit_retention_min" :max="info?.audit_retention_max"
            :placeholder="String(info?.audit_retention_default ?? '')"
            class="w-28 min-w-0 px-3 py-1.5 bg-ms-well border border-ms-border rounded text-sm text-ms-text focus:outline-none focus:border-brand-primary" />
          <span class="text-xs text-ms-muted">days</span>
          <button @click="saveAuditRetention"
            :disabled="savingAudit || auditDays.trim() === (info?.audit_retention_days ? String(info.audit_retention_days) : '')"
            class="px-3 py-1.5 bg-brand-primary hover:bg-brand-accent disabled:opacity-50 text-ms-on-primary text-sm font-medium rounded transition-colors">
            Save
          </button>
        </div>
        <p class="mt-1 text-xs text-ms-muted">
          Leave empty for the platform default ({{ info?.audit_retention_default }} days).
          Between {{ info?.audit_retention_min }} and {{ info?.audit_retention_max }} days.
          Entries older than this are archived and then removed.
        </p>

        <!-- Send budget. What the Hub sends to a device goes out over THIS
             tenant's own Cloudloop, Twilio or Rock7 account, so the limit is a
             safety net for the tenant's own bill and the tenant sets it. -->
        <p class="block text-xs text-ms-muted2 mt-4 mb-1">Messages the Hub may send to one device</p>
        <div class="flex flex-wrap gap-2 items-center">
          <label for="tenant-cap-daily" class="sr-only">Per day</label>
          <input id="tenant-cap-daily" v-model="capDaily" type="number" inputmode="numeric"
            :min="info?.ratelimit_daily_default" :max="info?.ratelimit_daily_max"
            :placeholder="String(info?.ratelimit_daily_default ?? '')"
            class="w-28 min-w-0 px-3 py-1.5 bg-ms-well border border-ms-border rounded text-sm text-ms-text focus:outline-none focus:border-brand-primary" />
          <span class="text-xs text-ms-muted">per day</span>
          <label for="tenant-cap-monthly" class="sr-only">Per month</label>
          <input id="tenant-cap-monthly" v-model="capMonthly" type="number" inputmode="numeric"
            :min="info?.ratelimit_monthly_default || 1" :max="info?.ratelimit_monthly_max"
            :placeholder="info?.ratelimit_monthly_default ? String(info.ratelimit_monthly_default) : 'no limit'"
            class="w-28 min-w-0 px-3 py-1.5 bg-ms-well border border-ms-border rounded text-sm text-ms-text focus:outline-none focus:border-brand-primary" />
          <span class="text-xs text-ms-muted">per month</span>
          <button @click="saveSendCaps" :disabled="savingCaps || capsUnchanged()"
            class="px-3 py-1.5 bg-brand-primary hover:bg-brand-accent disabled:opacity-50 text-ms-on-primary text-sm font-medium rounded transition-colors">
            Save
          </button>
        </div>
        <p class="mt-1 text-xs text-ms-muted">
          Counted per device. These messages go out over your own satellite and SMS accounts, so this
          is a guard on your own bill: a stuck script stops here instead of at your carrier.
          Leave empty for the platform default ({{ info?.ratelimit_daily_default }} per day,
          {{ info?.ratelimit_monthly_default ? info.ratelimit_monthly_default + ' per month' : 'no monthly limit' }}).
          Up to {{ info?.ratelimit_daily_max }} per day. An SOS is never counted and never held back.
        </p>

        <!-- Out-of-band command policy (MESHSAT-1121). These commands go to this
             tenant's OWN field kit over a bearer this tenant pays for, so the
             rate and the reply timeouts are its call, inside platform bounds. -->
        <h4 class="text-xs font-semibold text-ms-text2 mt-5 mb-1">Out-of-band commands</h4>
        <p class="text-xs text-ms-muted mb-2">
          How hard the Hub may drive your field kit when MQTT is down and commands go over SMS or
          satellite. Every command is encrypted; that is not optional.
        </p>
        <div class="grid grid-cols-1 sm:grid-cols-3 gap-3">
          <div>
            <label for="tenant-oob-max" class="block text-xs text-ms-muted2 mb-1">Commands per hour</label>
            <input id="tenant-oob-max" v-model="oobMax" type="number" inputmode="numeric"
              :min="info?.oob_max_per_hour_min" :max="info?.oob_max_per_hour_max"
              :placeholder="String(info?.oob_max_per_hour_default ?? '')"
              class="w-full min-w-0 px-3 py-1.5 bg-ms-well border border-ms-border rounded text-sm text-ms-text focus:outline-none focus:border-brand-primary" />
          </div>
          <div>
            <label for="tenant-oob-sms" class="block text-xs text-ms-muted2 mb-1">SMS reply wait (s)</label>
            <input id="tenant-oob-sms" v-model="oobSms" type="number" inputmode="numeric"
              :min="info?.oob_timeout_min" :max="info?.oob_timeout_max"
              :placeholder="String(info?.oob_sms_timeout_default ?? '')"
              class="w-full min-w-0 px-3 py-1.5 bg-ms-well border border-ms-border rounded text-sm text-ms-text focus:outline-none focus:border-brand-primary" />
          </div>
          <div>
            <label for="tenant-oob-sat" class="block text-xs text-ms-muted2 mb-1">Satellite reply wait (s)</label>
            <input id="tenant-oob-sat" v-model="oobSat" type="number" inputmode="numeric"
              :min="info?.oob_timeout_min" :max="info?.oob_timeout_max"
              :placeholder="String(info?.oob_sat_timeout_default ?? '')"
              class="w-full min-w-0 px-3 py-1.5 bg-ms-well border border-ms-border rounded text-sm text-ms-text focus:outline-none focus:border-brand-primary" />
          </div>
        </div>
        <div class="flex gap-2 items-center mt-2">
          <button @click="saveOOB" :disabled="savingOOB"
            class="px-3 py-1.5 bg-brand-primary hover:bg-brand-accent disabled:opacity-50 text-ms-on-primary text-sm font-medium rounded transition-colors">
            Save
          </button>
        </div>
        <p class="mt-1 text-xs text-ms-muted">
          Leave a box empty for the platform default. A satellite reply waits for a pass, so it is
          normally much longer than an SMS one.
        </p>
        <dl class="mt-3 text-xs text-ms-muted grid grid-cols-[auto_1fr] gap-x-3 gap-y-1">
          <dt>Slug</dt><dd class="font-mono text-ms-text">{{ info?.slug }}</dd>
          <dt>Plan</dt><dd class="text-ms-text capitalize">{{ info?.plan }}</dd>
          <dt>Status</dt><dd class="text-ms-text capitalize">{{ info?.status }}</dd>
        </dl>
      </div>

      <!-- Plan and usage. The ceiling applies to registering new devices and
           bridges; everything already registered keeps working whatever the
           plan says, which is what the note below tells the reader. -->
      <div v-if="usage" class="border-t border-ms-border pt-4">
        <div class="flex flex-wrap items-center gap-2">
          <h4 class="text-sm font-medium text-ms-text">Plan</h4>
          <span class="px-2 py-0.5 rounded text-[11px] font-medium border capitalize"
            :class="usageBadge">
            {{ usage.plan }} &middot;
            <template v-if="unlimited">{{ usage.used }} devices</template>
            <template v-else>{{ usage.used }} / {{ usage.limit }} devices</template>
          </span>
          <a v-if="!billingInHub && usage.upgrade_url" :href="usage.upgrade_url" target="_blank" rel="noopener noreferrer"
            class="ml-auto px-3 py-1 bg-brand-primary hover:bg-brand-accent text-ms-on-primary text-xs font-medium rounded transition-colors">
            Support &amp; upgrade
          </a>
        </div>

        <div v-if="!unlimited" class="mt-2 h-1.5 w-full bg-ms-well rounded overflow-hidden"
          role="progressbar" :aria-valuenow="usage.used" aria-valuemin="0" :aria-valuemax="usage.limit">
          <div class="h-full transition-all"
            :class="usage.remaining === 0 ? 'bg-ms-warning' : 'bg-brand-primary'"
            :style="{ width: Math.min(100, (usage.used / Math.max(1, usage.limit)) * 100) + '%' }"></div>
        </div>

        <p class="text-[11px] text-ms-muted mt-2">
          {{ usage.devices }} device<span v-if="usage.devices !== 1">s</span> and
          {{ usage.bridges }} bridge<span v-if="usage.bridges !== 1">s</span> registered.
          The limit applies to adding new ones. Everything already registered keeps reporting,
          and an SOS is never affected by your plan.
        </p>

        <ul v-if="usage.tiers?.length" class="mt-3 text-xs divide-y divide-ms-border">
          <li v-for="tier in usage.tiers" :key="tier.plan"
            class="py-1.5 flex items-center gap-2"
            :class="tier.current ? 'text-ms-text' : 'text-ms-muted'">
            <span class="capitalize w-16">{{ tier.plan }}</span>
            <span class="font-mono">{{ tier.devices === -1 ? 'custom' : tier.devices + ' devices' }}</span>
            <span v-if="tier.current" class="ml-auto px-1.5 py-0.5 rounded text-[10px] border bg-emerald-900/50 text-emerald-300 border-emerald-700/50">current</span>
          </li>
        </ul>

        <div v-if="billingInHub" class="mt-3 flex flex-wrap items-center gap-2">
          <button v-for="tier in buyable" :key="tier.plan" type="button"
            :disabled="checkoutBusy || tier.current"
            @click="subscribe(tier.plan)"
            class="px-3 py-1.5 bg-brand-primary hover:bg-brand-accent text-ms-on-primary text-xs font-medium rounded transition-colors disabled:opacity-50 disabled:cursor-not-allowed">
            {{ tier.current ? planName(tier.plan) + ' (current)' : 'Subscribe to ' + planName(tier.plan) }}
          </button>
          <button v-if="usage.billing?.manageable" type="button" :disabled="checkoutBusy"
            @click="manageBilling"
            class="px-3 py-1.5 border border-ms-border rounded text-xs text-ms-text2 hover:text-ms-text hover:border-ms-border-light transition-colors disabled:opacity-50">
            Manage billing
          </button>
          <button v-if="usage.billing?.donatable" type="button" :disabled="checkoutBusy"
            @click="donate"
            class="px-3 py-1.5 border border-ms-border rounded text-xs text-ms-text2 hover:text-ms-text hover:border-ms-border-light transition-colors disabled:opacity-50">
            Donate
          </button>
          <p class="w-full text-[11px] text-ms-muted mt-1">
            Payment is handled by Stripe. Cancel or change your card whenever you like. Your
            devices keep reporting either way, and an SOS is never affected by billing.
          </p>
          <p v-if="usage.billing?.donatable" class="w-full text-[11px] text-ms-muted">
            A donation is support rather than a purchase: it changes no plan and unlocks nothing.
          </p>
        </div>
      </div>

      <div>
        <label for="invite-email" class="block text-xs text-ms-muted2 mb-1">Invite by email</label>
        <div class="flex flex-wrap gap-2">
          <input id="invite-email" v-model="inviteEmail" type="email" placeholder="person@example.org" autocomplete="off"
            class="flex-1 min-w-0 px-3 py-1.5 bg-ms-well border border-ms-border rounded text-sm text-ms-text placeholder-ms-muted focus:outline-none focus:border-brand-primary" />
          <select v-model="inviteRole" aria-label="Role"
            class="px-2 py-1.5 bg-ms-well border border-ms-border rounded text-sm text-ms-text">
            <option value="viewer">Viewer</option>
            <option value="operator">Operator</option>
            <option value="owner">Owner</option>
          </select>
          <button @click="sendInvite" :disabled="inviting || !inviteEmail.trim()"
            class="px-3 py-1.5 bg-brand-primary hover:bg-brand-accent disabled:opacity-50 text-ms-on-primary text-sm font-medium rounded transition-colors">
            Invite
          </button>
        </div>
        <p class="text-[11px] text-ms-muted mt-1">The invite is matched to the verified email of their MeshSat ID on first sign-in.</p>

        <ul v-if="invites.length" class="mt-3 divide-y divide-ms-border text-sm">
          <li v-for="inv in invites" :key="inv.id" class="py-1.5 flex flex-wrap items-center gap-x-3 gap-y-1">
            <span class="basis-full sm:basis-auto sm:flex-1 truncate text-ms-text">{{ inv.email }}</span>
            <span class="text-xs text-ms-muted capitalize">{{ inv.role }}</span>
            <span class="text-xs text-ms-muted">{{ inv.accepted_at ? 'accepted' : 'expires ' + fmt(inv.expires_at) }}</span>
            <button v-if="!inv.accepted_at" @click="revoke(inv)" class="text-xs text-ms-muted hover:text-ms-error">Revoke</button>
          </li>
        </ul>
        <p v-else class="mt-3 text-xs text-ms-muted">No pending invites.</p>
      </div>

      <div class="pt-4 border-t border-ms-border">
        <h4 class="text-sm font-medium text-ms-text">Your data</h4>
        <p class="text-[11px] text-ms-muted mt-1 mb-2">
          Take a copy of everything in this account, or close it. Closing blocks access straight away and
          erases the data after {{ info?.purge_grace_days || 30 }} days. Until then it can be undone by asking us.
        </p>
        <div class="flex flex-wrap gap-2">
          <button @click="exportData" :disabled="exporting"
            class="px-3 py-1.5 bg-ms-well hover:bg-ms-card disabled:opacity-50 border border-ms-border text-ms-text text-sm font-medium rounded transition-colors">
            {{ exporting ? 'Preparing…' : 'Download my data' }}
          </button>
          <button v-if="!confirmClose" @click="confirmClose = true; confirmText = ''"
            class="px-3 py-1.5 bg-transparent hover:bg-ms-well border border-ms-border text-ms-muted hover:text-ms-error text-sm rounded transition-colors">
            Close this account
          </button>
        </div>

        <div v-if="confirmClose" class="mt-3 p-3 rounded border border-ms-error/50 bg-ms-error/5">
          <p class="text-sm text-ms-text">This closes <span class="font-medium">{{ info?.name || info?.slug }}</span>.</p>
          <ul class="text-[11px] text-ms-muted mt-1 mb-2 list-disc list-inside space-y-0.5">
            <li>Everyone in the account loses access immediately.</li>
            <li>Devices stop being manageable here. They do not stop transmitting.</li>
            <li>A paid plan does not cancel itself. Cancel it in Manage billing first, or it keeps charging.</li>
            <li>After the grace period the data is destroyed and cannot be recovered.</li>
          </ul>
          <label for="close-confirm" class="block text-xs text-ms-muted2 mb-1">
            Type <span class="font-mono text-ms-text">{{ info?.slug }}</span> to confirm
          </label>
          <div class="flex flex-wrap gap-2">
            <input id="close-confirm" v-model="confirmText" autocomplete="off"
              class="flex-1 min-w-0 px-3 py-1.5 bg-ms-well border border-ms-border rounded text-sm text-ms-text focus:outline-none focus:border-ms-error" />
            <button @click="closeAccount" :disabled="closing || confirmText !== info?.slug"
              class="px-3 py-1.5 bg-ms-error hover:opacity-90 disabled:opacity-40 text-white text-sm font-medium rounded transition-colors">
              {{ closing ? 'Closing…' : 'Close account' }}
            </button>
            <button @click="confirmClose = false" class="px-3 py-1.5 text-sm text-ms-muted hover:text-ms-text">Cancel</button>
          </div>
        </div>
      </div>
    </div>
    <p v-if="error" class="text-ms-error text-sm mt-3">{{ error }}</p>
  </div>
</template>
