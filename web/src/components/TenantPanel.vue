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
const saving = ref(false)
const invites = ref([])
const inviteEmail = ref('')
const inviteRole = ref('operator')
const inviting = ref(false)
const error = ref('')
const usage = ref(null)
const copied = ref(false)

// Devices and bridges share one ceiling, because that is the number somebody
// can count against their own kit. -1 means the plan has no ceiling.
const unlimited = computed(() => usage.value?.limit === -1)
const usageBadge = computed(() => {
  if (!usage.value || unlimited.value) return 'bg-sky-900/50 text-sky-300 border-sky-700/50'
  if (usage.value.over_limit) return 'bg-amber-900/50 text-amber-300 border-amber-700/50'
  if (usage.value.remaining === 0) return 'bg-amber-900/50 text-amber-300 border-amber-700/50'
  return 'bg-emerald-900/50 text-emerald-300 border-emerald-700/50'
})

async function copyClaimCode() {
  if (!usage.value?.claim_code) return
  try {
    await navigator.clipboard.writeText(usage.value.claim_code)
    copied.value = true
    setTimeout(() => { copied.value = false }, 2000)
  } catch {
    toast.error('Could not copy. Select the code and copy it by hand.')
  }
}

async function load() {
  try {
    info.value = await tenantApi.get()
    name.value = info.value.name || ''
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
          <a v-if="usage.upgrade_url" :href="usage.upgrade_url" target="_blank" rel="noopener noreferrer"
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

        <div v-if="usage.claim_code" class="mt-3">
          <label class="block text-xs text-ms-muted2 mb-1">Your claim code</label>
          <div class="flex flex-wrap items-center gap-2">
            <code class="px-2 py-1 bg-ms-well border border-ms-border rounded font-mono text-sm text-ms-text tracking-widest">{{ usage.claim_code }}</code>
            <button @click="copyClaimCode" type="button"
              class="px-2 py-1 border border-ms-border rounded text-xs text-ms-text2 hover:text-ms-text hover:border-ms-border-light transition-colors">
              {{ copied ? 'Copied' : 'Copy' }}
            </button>
          </div>
          <p class="text-[11px] text-ms-muted mt-1">
            Put this in the message when you pay, so the payment reaches this account.
            People often pay from a different address than they signed up with.
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
    </div>
    <p v-if="error" class="text-ms-error text-sm mt-3">{{ error }}</p>
  </div>
</template>
