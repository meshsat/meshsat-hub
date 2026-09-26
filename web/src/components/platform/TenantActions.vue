<script setup>
// What an operator can do TO a tenant: open its workspace behind the owner's
// support PIN, suspend or reactivate it, close it. Every one is confirmed;
// closing needs the slug typed back (TenantPanel's own pattern).
import { ref, computed } from 'vue'
import { admin as adminApi } from '../../api/client'
import { useAuthStore } from '../../stores/auth'
import { useToastStore } from '../../stores/toast'
import { formatWhen } from '../../utils/time'
import ConfirmDialog from './ConfirmDialog.vue'

const props = defineProps({
  tenant: { type: Object, required: true },
})
const emit = defineEmits(['changed'])
const auth = useAuthStore()
const toast = useToastStore()

const closed = computed(() => !!props.tenant.deleted_at)
const suspended = computed(() => props.tenant.status === 'suspended')
const grant = computed(() => props.tenant.support_access || {})
const canOpen = computed(() => !!grant.value.active && !closed.value && !suspended.value)

const openReason = computed(() => {
  if (closed.value) return 'This account is closed.'
  if (suspended.value) return 'This account is suspended.'
  if (!grant.value.active) return 'The customer has not granted support access.'
  return ''
})

// --- Open as this tenant (PIN)
const pinOpen = ref(false)
const pin = ref('')
const pinBusy = ref(false)
const pinError = ref('')
function startOpen() { pin.value = ''; pinError.value = ''; pinOpen.value = true }
async function confirmOpen() {
  if (pin.value.length < 10) return
  pinBusy.value = true
  pinError.value = ''
  try {
    await auth.startViewAs(props.tenant, pin.value)
    pinOpen.value = false
  } catch (e) {
    const code = e?.body?.code || e?.code || ''
    if (e?.status === 401 && code === 'wrong_pin') {
      const left = e.body?.attempts_left
      pinError.value = left === 1 ? 'Wrong PIN, 1 attempt left' : `Wrong PIN, ${left ?? 'no'} attempts left`
    } else if (e?.status === 403 && code === 'support_access_locked') {
      pinError.value = 'Support access is locked after too many wrong PINs. The customer has to grant it again.'
    } else if (e?.status === 403 && code === 'support_access_required') {
      pinError.value = 'The customer has not granted support access, or the grant has expired.'
    } else if (e?.status === 409) {
      pinError.value = 'This tenant is suspended or closed.'
    } else if (e?.status === 404) {
      pinError.value = 'This tenant no longer exists.'
    } else if (e?.status === 400) {
      pinError.value = 'The platform tenant cannot be opened this way.'
    } else {
      pinError.value = e?.message || 'Could not open the workspace'
    }
    emit('changed')
  } finally {
    pinBusy.value = false
  }
}

// --- Suspend / reactivate
const statusOpen = ref(false)
const statusBusy = ref(false)
async function confirmStatus() {
  statusBusy.value = true
  try {
    await adminApi.tenants.update(props.tenant.id, { status: suspended.value ? 'active' : 'suspended' })
    toast.success(suspended.value ? 'Account reactivated' : 'Account suspended')
    statusOpen.value = false
    emit('changed')
  } catch (e) {
    toast.error(e?.message || 'Could not change the status')
  } finally {
    statusBusy.value = false
  }
}

// --- Close
const closeOpen = ref(false)
const closeText = ref('')
const closeBusy = ref(false)
function startClose() { closeText.value = ''; closeOpen.value = true }
async function confirmClose() {
  if (closeText.value !== props.tenant.slug) return
  closeBusy.value = true
  try {
    await adminApi.tenants.close(props.tenant.id)
    toast.success('Account closed. It can be recovered during the grace period.')
    closeOpen.value = false
    emit('changed')
  } catch (e) {
    toast.error(e?.message || 'Could not close the account')
  } finally {
    closeBusy.value = false
  }
}
</script>

<template>
  <section class="ms-panel" aria-labelledby="actions-h">
    <div class="ms-panel-head"><h2 id="actions-h" class="ms-h2">Actions</h2></div>
    <div class="px-4 py-4 space-y-4 text-[13px]">
      <div class="flex flex-wrap items-center gap-3">
        <button type="button" class="ms-btn" :disabled="!canOpen" @click="startOpen">Open as this tenant</button>
        <span v-if="openReason" class="text-ms-muted">{{ openReason }}</span>
        <span v-else class="text-ms-muted">
          Granted by {{ grant.created_by_email || 'the owner' }}, expires {{ formatWhen(grant.expires_at, '') }}<template v-if="grant.used_at">, last opened by {{ grant.used_by_email || 'support' }} at {{ formatWhen(grant.used_at, '') }}</template>.
        </span>
      </div>
      <div v-if="!closed" class="flex flex-wrap items-center gap-3 pt-3 border-t border-ms-border">
        <button type="button" class="ms-btn-danger" @click="statusOpen = true">{{ suspended ? 'Reactivate' : 'Suspend' }}</button>
        <span class="text-ms-muted">{{ suspended ? 'Suspended: nobody in the account can sign in. Devices keep reporting.' : 'Suspending blocks sign-in for everyone in the account. Devices keep reporting and an SOS still goes through.' }}</span>
        <button type="button" class="ms-btn-danger sm:ml-auto" @click="startClose">Close account</button>
      </div>
    </div>

    <ConfirmDialog :open="pinOpen" title="Open as this tenant" label="Open workspace" variant="plain" :busy="pinBusy" :disabled="pin.length < 10" @confirm="confirmOpen" @cancel="pinOpen = false">
      <p>Enter the PIN the customer shared with you. Everything you do inside <span class="font-medium text-ms-text">{{ tenant.name || tenant.slug }}</span> is written to their audit log under your name.</p>
      <label class="block">
        <span class="ms-label">Support PIN</span>
        <input v-model="pin" type="password" class="ms-input w-full mt-1" minlength="10" maxlength="128" autocomplete="one-time-code" @keydown.enter.prevent="confirmOpen" />
      </label>
      <p v-if="pinError" class="text-ms-error" role="alert">{{ pinError }}</p>
    </ConfirmDialog>

    <ConfirmDialog :open="statusOpen" :title="suspended ? 'Reactivate this account?' : 'Suspend this account?'" :label="suspended ? 'Reactivate' : 'Suspend'" :busy="statusBusy" @confirm="confirmStatus" @cancel="statusOpen = false">
      <p v-if="suspended">Everyone in <span class="font-medium text-ms-text">{{ tenant.name || tenant.slug }}</span> can sign in again.</p>
      <p v-else>Nobody in <span class="font-medium text-ms-text">{{ tenant.name || tenant.slug }}</span> can sign in until it is reactivated. Its devices keep reporting and an SOS is never held back.</p>
    </ConfirmDialog>

    <ConfirmDialog :open="closeOpen" title="Close this account?" label="Close account" :busy="closeBusy" :disabled="closeText !== tenant.slug" @confirm="confirmClose" @cancel="closeOpen = false">
      <p>This closes <span class="font-medium text-ms-text">{{ tenant.name || tenant.slug }}</span>.</p>
      <ul class="list-disc list-inside text-xs text-ms-muted space-y-0.5">
        <li>Everyone in the account loses access immediately.</li>
        <li>Devices stop being manageable here. They do not stop transmitting.</li>
        <li>A paid plan does not cancel itself; cancel it in Stripe first, or it keeps charging.</li>
        <li>After {{ tenant.purge_grace_days || 30 }} days the data is destroyed and cannot be recovered.</li>
      </ul>
      <label class="block">
        <span class="ms-label">Type <span class="ms-id text-ms-text">{{ tenant.slug }}</span> to confirm</span>
        <input v-model="closeText" class="ms-input w-full mt-1" autocomplete="off" />
      </label>
    </ConfirmDialog>
  </section>
</template>
