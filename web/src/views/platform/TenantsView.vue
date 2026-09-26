<script setup>
// Every tenant on the platform, with the numbers a person on support needs
// before opening one. The search is client-side across id, slug, name and
// owner email; closed accounts are hidden unless asked for.
import { ref, computed, onMounted, watch } from 'vue'
import { useRouter } from 'vue-router'
import { admin as adminApi } from '../../api/client'
import { formatWhen } from '../../utils/time'
import EmptyState from '../../components/EmptyState.vue'

const router = useRouter()
const rows = ref([])
const loading = ref(true)
const error = ref('')
const q = ref('')
const showClosed = ref(false)

async function load() {
  loading.value = true
  error.value = ''
  try {
    const res = await adminApi.tenants.list('', showClosed.value)
    rows.value = Array.isArray(res) ? res : (res?.tenants || [])
  } catch (e) {
    error.value = e?.message || 'Could not load the tenants'
  } finally {
    loading.value = false
  }
}
watch(showClosed, load)

const filtered = computed(() => {
  const needle = q.value.trim().toLowerCase()
  const list = rows.value.filter((t) => showClosed.value || !t.deleted_at)
  if (!needle) return list
  return list.filter((t) => [t.id, t.slug, t.name, t.owner_email].some((v) => String(v || '').toLowerCase().includes(needle)))
})

function statusOf(t) {
  if (t.deleted_at) return 'closed'
  return t.status || ''
}
function abnormal(t) {
  const s = statusOf(t)
  return s === 'closed' || s === 'suspended'
}
function usageText(t) {
  if (t.limit === -1) return `${t.used ?? 0}, no ceiling`
  return `${t.used ?? 0} of ${t.limit ?? 0}`
}
function open(t) { router.push({ name: 'platformTenant', params: { id: t.id } }) }

onMounted(load)
</script>

<template>
  <div class="ms-page max-w-7xl">
    <div class="ms-page-head">
      <div>
        <h1 class="ms-h1">Tenants</h1>
        <p class="ms-lede">Every account on this Hub. Open one for its plan, usage, users and audit trail, or to work inside it with the owner's support access.</p>
      </div>
      <div class="flex flex-wrap items-center gap-3">
        <label class="sr-only" for="tenant-q">Search tenants</label>
        <input id="tenant-q" v-model="q" type="search" class="ms-input w-64 max-w-full" placeholder="Search id, slug, name or owner" autocomplete="off" />
        <label class="inline-flex items-center gap-2 text-[13px] text-ms-text2">
          <input v-model="showClosed" type="checkbox" class="accent-ms-primary" />
          Show closed
        </label>
        <button type="button" class="ms-btn" :disabled="loading" @click="load">Refresh</button>
      </div>
    </div>

    <div v-if="error" class="ms-alert mb-5">{{ error }}</div>

    <div class="ms-panel">
      <div v-if="loading" class="px-4 py-8 text-sm text-ms-muted">Loading tenants.</div>
      <EmptyState v-else-if="!filtered.length" icon="users" title="No tenants match"
        :message="q ? 'Nothing matches that search.' : (showClosed ? 'There are no tenants on this Hub.' : 'No open tenants. Tick Show closed to include closed accounts.')" />
      <div v-else class="overflow-x-auto">
        <table class="ms-table" data-testid="tenants">
          <thead>
            <tr>
              <th>Tenant</th>
              <th>Owner</th>
              <th>Plan</th>
              <th>Status</th>
              <th>Created</th>
              <th class="text-right">Used</th>
              <th>Country</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="t in filtered" :key="t.id" class="cursor-pointer hover:bg-ms-well/50" @click="open(t)">
              <td class="!h-14 max-w-0 w-full min-w-[14rem]">
                <RouterLink :to="{ name: 'platformTenant', params: { id: t.id } }" class="block min-w-0 rounded" @click.stop>
                  <div class="text-[13px] font-medium text-ms-text truncate">{{ t.name || t.slug || t.id }}</div>
                  <div class="ms-id text-ms-muted truncate">{{ t.slug || t.id }}</div>
                </RouterLink>
              </td>
              <td class="ms-id whitespace-nowrap">{{ t.owner_email || '' }}</td>
              <td class="whitespace-nowrap">{{ t.plan }}</td>
              <td class="whitespace-nowrap">
                <span v-if="abnormal(t)" class="ms-chip !text-ms-warning !border-ms-warning/50">{{ statusOf(t) }}</span>
                <template v-else>{{ statusOf(t) }}</template>
              </td>
              <td class="ms-num whitespace-nowrap text-ms-muted">{{ formatWhen(t.created_at, '') }}</td>
              <td class="ms-num whitespace-nowrap text-right" :class="t.over_limit ? 'text-ms-warning' : ''">{{ usageText(t) }}</td>
              <td class="whitespace-nowrap">{{ t.billing_country || '' }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
  </div>
</template>
