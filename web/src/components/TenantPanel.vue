<script setup>
// Tenant panel (Settings): name and invites for the signed-in tenant.
// Owner-only. Talks to /api/tenant (MR 16); stays hidden while that API
// is absent so the panel can ship ahead of it.
import { ref, onMounted } from 'vue'
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
            class="flex-1 px-3 py-1.5 bg-ms-well border border-ms-border rounded text-sm text-ms-text focus:outline-none focus:border-brand-primary" />
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

      <div>
        <label for="invite-email" class="block text-xs text-ms-muted2 mb-1">Invite by email</label>
        <div class="flex gap-2">
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
          <li v-for="inv in invites" :key="inv.id" class="py-1.5 flex items-center gap-3">
            <span class="flex-1 truncate text-ms-text">{{ inv.email }}</span>
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
