<script setup>
// Account requests (MESHSAT-978, moved out of Settings). Approving activates
// the account in the identity provider and grants the role; their tenant is
// created on first sign-in. Rejecting deletes the request and, when a reason
// is given, mails it to them. Availability is read off the HTTP status: a
// 502/503 means the Hub has no identity provider to ask.
import { ref, computed, onMounted } from 'vue'
import { admin as adminApi } from '../../api/client'
import { usePlatformStore } from '../../stores/platform'
import { useToastStore } from '../../stores/toast'
import { formatWhen } from '../../utils/time'
import ConfirmDialog from '../../components/platform/ConfirmDialog.vue'

const platform = usePlatformStore()
const toast = useToastStore()

const loading = ref(true)
const pending = ref([])
const roles = ref({})
const busy = ref(0)
const listStatus = ref(0)
const listError = ref('')
const history = ref([])
const historyError = ref('')

const noProvider = computed(() => listStatus.value === 502 || listStatus.value === 503)
const visible = computed(() => pending.value.filter((s) => !s.probe))

// The policy stamps an RFC 3339 instant. Show an exact moment in a stated
// zone rather than a bare date: this is a consent record.
function acceptedOn(v) {
  const d = new Date(v)
  if (isNaN(d)) return v
  return d.toLocaleString(undefined, {
    weekday: 'short', day: '2-digit', month: 'short', year: 'numeric',
    hour: '2-digit', minute: '2-digit', timeZoneName: 'short',
  })
}

function facts(s) {
  return [
    ['Username', s.username, ''],
    ['Organisation', s.organisation, ''],
    ['Country', s.country, ''],
    ['Callsign', s.callsign, 'ms-id'],
    ['Matrix', s.matrix_id, 'ms-id'],
    ['Hardware', s.hardware, ''],
    ['Signed up from', s.signup_ip, 'ms-id'],
    ['Requested', s.created ? formatWhen(s.created) : '', 'ms-num'],
    ['Accepted terms', s.terms_accepted_at ? acceptedOn(s.terms_accepted_at) : '', ''],
  ].filter(([, v]) => !!v)
}

async function load() {
  loading.value = true
  listError.value = ''
  try {
    const res = await adminApi.signups.list()
    pending.value = res?.signups || []
    for (const s of pending.value) if (!roles.value[s.pk]) roles.value[s.pk] = 'owner'
    listStatus.value = 200
  } catch (e) {
    listStatus.value = e?.status || 0
    if (!(e?.status === 502 || e?.status === 503)) listError.value = e?.message || 'Could not load the requests'
  } finally {
    loading.value = false
  }
}

async function loadHistory() {
  historyError.value = ''
  try {
    const res = await adminApi.signups.history(100)
    history.value = Array.isArray(res) ? res : []
  } catch (e) {
    historyError.value = e?.message || 'Could not load the decision history'
  }
}

async function approve(s) {
  busy.value = s.pk
  try {
    const res = await adminApi.signups.approve(s.pk, roles.value[s.pk] || 'owner')
    toast.success(`${res?.email || s.email} approved as ${res?.role || roles.value[s.pk]}. They can sign in now.`)
    await Promise.all([load(), loadHistory()])
    platform.nudge()
  } catch (e) {
    toast.error(e?.message || 'Approval failed')
  } finally {
    busy.value = 0
  }
}

// Reject: a dialog with an optional reason, sent to them in the mail.
const rejecting = ref(null)
const reason = ref('')
function openReject(s) { rejecting.value = s; reason.value = '' }
async function reject() {
  const s = rejecting.value
  if (!s) return
  busy.value = s.pk
  try {
    await adminApi.signups.reject(s.pk, reason.value.trim())
    toast.success(`${s.email} rejected`)
    rejecting.value = null
    await Promise.all([load(), loadHistory()])
    platform.nudge()
  } catch (e) {
    toast.error(e?.message || 'Rejection failed')
  } finally {
    busy.value = 0
  }
}

function decisionLabel(h) {
  return h.action === 'signup_approved' ? 'Approved' : h.action === 'signup_rejected' ? 'Rejected' : h.action
}

onMounted(() => { load(); loadHistory() })
</script>

<template>
  <div class="ms-page max-w-6xl">
    <div class="ms-page-head">
      <div>
        <h1 class="ms-h1">Account requests</h1>
        <p class="ms-lede">People who asked for a MeshSat ID. Approving activates the account and grants the role; their tenant is created on first sign-in. Rejecting deletes the request.</p>
      </div>
      <button type="button" class="ms-btn" :disabled="loading" @click="load(); loadHistory()">Refresh</button>
    </div>

    <div v-if="noProvider" class="ms-note mb-5" data-testid="no-provider">
      The identity provider is not configured on this Hub; approve with the script.
    </div>
    <div v-else-if="listError" class="ms-alert mb-5">{{ listError }}</div>

    <section class="ms-panel mb-5" aria-labelledby="pending-h">
      <div class="ms-panel-head">
        <h2 id="pending-h" class="ms-h2">Waiting</h2>
        <span class="text-xs text-ms-muted ms-num">{{ visible.length }}</span>
      </div>
      <div v-if="loading" class="px-4 py-8 text-sm text-ms-muted">Loading requests.</div>
      <div v-else-if="noProvider" class="px-4 py-8 text-sm text-ms-muted">Nothing can be listed without the identity provider.</div>
      <div v-else-if="!visible.length" class="px-4 py-8 text-sm text-ms-muted">Nothing waiting.</div>
      <ul v-else class="divide-y divide-ms-border">
        <li v-for="s in visible" :key="s.pk" class="px-4 py-4">
          <div class="flex flex-wrap items-baseline gap-x-2 gap-y-1">
            <span class="text-sm text-ms-text font-medium">{{ s.name || s.username }}</span>
            <span class="ms-id text-ms-muted2">{{ s.email }}</span>
            <span v-if="!s.email_verified" class="ms-chip !text-ms-warning !border-ms-warning/50">email not verified</span>
          </div>
          <dl class="mt-2 grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-x-6 gap-y-1 text-xs">
            <div v-for="[k, v, cls] in facts(s)" :key="k" class="min-w-0">
              <dt class="inline text-ms-muted">{{ k }}: </dt>
              <dd class="inline text-ms-text2 break-words" :class="cls">{{ v }}</dd>
            </div>
          </dl>
          <p v-if="s.intended_use" class="mt-2 text-xs text-ms-text2 max-w-[80ch]">{{ s.intended_use }}</p>
          <div class="mt-3 flex flex-wrap items-center gap-2">
            <label class="sr-only" :for="`role-${s.pk}`">Role for {{ s.email }}</label>
            <select :id="`role-${s.pk}`" v-model="roles[s.pk]" class="ms-input">
              <option value="owner">owner</option>
              <option value="operator">operator</option>
              <option value="viewer">viewer</option>
            </select>
            <button type="button" class="ms-btn" :disabled="busy === s.pk" @click="approve(s)">
              {{ busy === s.pk ? 'Working' : 'Approve' }}
            </button>
            <button type="button" class="ms-btn-danger" :disabled="busy === s.pk" @click="openReject(s)">Reject</button>
          </div>
        </li>
      </ul>
    </section>

    <section class="ms-panel" aria-labelledby="history-h">
      <div class="ms-panel-head">
        <h2 id="history-h" class="ms-h2">Decision history</h2>
        <span class="text-xs text-ms-muted ms-num">{{ history.length }}</span>
      </div>
      <div v-if="historyError" class="px-4 py-3 text-[13px] text-ms-error">{{ historyError }}</div>
      <div v-else-if="!history.length" class="px-4 py-8 text-sm text-ms-muted">No decisions recorded yet.</div>
      <div v-else class="overflow-x-auto">
        <table class="ms-table">
          <thead>
            <tr>
              <th>Time</th>
              <th>Decision</th>
              <th>Email</th>
              <th>Role or reason</th>
              <th>By</th>
              <th>IP</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="h in history" :key="h.id">
              <td class="ms-num whitespace-nowrap text-ms-muted">{{ formatWhen(h.created_at, '') }}</td>
              <td>{{ decisionLabel(h) }}</td>
              <td class="ms-id">{{ h.email }}</td>
              <td class="max-w-[40ch] truncate" :title="h.action === 'signup_rejected' ? h.reason : h.role">
                {{ h.action === 'signup_rejected' ? (h.reason || 'no reason given') : h.role }}
              </td>
              <td class="ms-id">{{ h.actor }}</td>
              <td class="ms-id text-ms-muted">{{ h.ip || '' }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>

    <ConfirmDialog :open="!!rejecting" title="Reject this request?" label="Reject request" :busy="busy !== 0" @confirm="reject" @cancel="rejecting = null">
      <p>The request from <span class="font-medium text-ms-text">{{ rejecting?.email }}</span> is deleted from the identity provider. They can apply again.</p>
      <label class="block">
        <span class="ms-label">Reason</span>
        <textarea v-model="reason" class="ms-input w-full mt-1 !h-auto py-2" rows="3" maxlength="500" placeholder="Sent to them in the mail; optional"></textarea>
        <span class="block mt-1 text-xs text-ms-muted">Sent to them in the mail; optional. {{ reason.length }} of 500.</span>
      </label>
    </ConfirmDialog>
  </div>
</template>
