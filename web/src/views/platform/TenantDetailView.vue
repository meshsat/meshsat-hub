<script setup>
// One tenant, as the platform sees it: facts, usage, users, recent audit,
// the plan form and the operator actions. The panels are components under
// components/platform/; this view only loads and lays them out.
import { ref, computed, onMounted, watch } from 'vue'
import { useRoute } from 'vue-router'
import { admin as adminApi } from '../../api/client'
import { formatWhen } from '../../utils/time'
import TenantFacts from '../../components/platform/TenantFacts.vue'
import TenantPlanForm from '../../components/platform/TenantPlanForm.vue'
import TenantActions from '../../components/platform/TenantActions.vue'

const route = useRoute()
const id = computed(() => String(route.params.id || ''))

const tenant = ref(null)
const usage = ref(null)
const users = ref([])
const audit = ref([])
const loading = ref(true)
const error = ref('')
const errorStatus = ref(0)
const sideErrors = ref({ usage: '', users: '', audit: '' })

async function load() {
  loading.value = true
  error.value = ''
  try {
    tenant.value = await adminApi.tenants.get(id.value)
  } catch (e) {
    errorStatus.value = e?.status || 0
    error.value = e?.status === 404 ? 'No tenant with that id.' : (e?.message || 'Could not load the tenant')
    tenant.value = null
    loading.value = false
    return
  }
  const r = await Promise.allSettled([
    adminApi.tenants.usage(id.value),
    adminApi.tenants.users(id.value),
    adminApi.tenants.audit(id.value, 50),
  ])
  usage.value = r[0].status === 'fulfilled' ? r[0].value : null
  users.value = r[1].status === 'fulfilled' && Array.isArray(r[1].value) ? r[1].value : []
  audit.value = r[2].status === 'fulfilled' && Array.isArray(r[2].value) ? r[2].value : []
  sideErrors.value = {
    usage: r[0].status === 'rejected' ? (r[0].reason?.message || 'unavailable') : '',
    users: r[1].status === 'rejected' ? (r[1].reason?.message || 'unavailable') : '',
    audit: r[2].status === 'rejected' ? (r[2].reason?.message || 'unavailable') : '',
  }
  loading.value = false
}

function onSaved(res) {
  if (res && res.id) tenant.value = { ...tenant.value, ...res }
  load()
}

watch(id, load)
onMounted(load)
</script>

<template>
  <div class="ms-page max-w-6xl">
    <div class="ms-page-head">
      <div class="min-w-0">
        <p class="text-xs text-ms-muted mb-1"><RouterLink :to="{ name: 'platformTenants' }" class="hover:text-ms-text">Tenants</RouterLink></p>
        <h1 class="ms-h1 truncate">{{ tenant?.name || tenant?.slug || (loading ? 'Tenant' : 'Tenant') }}</h1>
        <p v-if="tenant" class="ms-lede"><span class="ms-id">{{ tenant.slug }}</span> <span class="text-ms-muted/70">·</span> <span class="ms-id">{{ tenant.id }}</span></p>
      </div>
      <button type="button" class="ms-btn" :disabled="loading" @click="load">Refresh</button>
    </div>

    <div v-if="error" class="ms-alert mb-5">{{ error }}</div>
    <div v-else-if="loading && !tenant" class="text-sm text-ms-muted py-8">Loading tenant.</div>

    <template v-if="tenant">
      <div v-if="tenant.deleted_at" class="ms-alert mb-5">
        Closed on {{ formatWhen(tenant.deleted_at, '') }}.
        <template v-if="tenant.purge_at">Its data is destroyed on {{ formatWhen(tenant.purge_at, '') }} unless it is recovered before then.</template>
      </div>
      <div v-else-if="tenant.status === 'suspended'" class="ms-note mb-5 !border-ms-warning/50">
        Suspended: nobody in this account can sign in. Its devices keep reporting.
      </div>

      <div class="grid grid-cols-1 lg:grid-cols-2 gap-5">
        <TenantFacts :tenant="tenant" :usage="usage" />
        <TenantPlanForm :tenant="tenant" :tiers="usage?.tiers || []" @saved="onSaved" />
        <TenantActions :tenant="tenant" @changed="load" />

        <section class="ms-panel lg:col-span-2" aria-labelledby="users-h">
          <div class="ms-panel-head">
            <h2 id="users-h" class="ms-h2">Users</h2>
            <span class="text-xs text-ms-muted ms-num">{{ users.length }}</span>
          </div>
          <div v-if="sideErrors.users" class="px-4 py-3 text-[13px] text-ms-error">{{ sideErrors.users }}</div>
          <div v-else-if="!users.length" class="px-4 py-6 text-sm text-ms-muted">No users in this account.</div>
          <div v-else class="overflow-x-auto">
            <table class="ms-table">
              <thead><tr><th>Email</th><th>Name</th><th>Role</th><th>Enabled</th><th>Last sign-in</th></tr></thead>
              <tbody>
                <tr v-for="u in users" :key="u.id">
                  <td class="ms-id">{{ u.email }}</td>
                  <td>{{ u.name }}</td>
                  <td>{{ u.role }}</td>
                  <td>{{ u.enabled ? 'yes' : 'no' }}</td>
                  <td class="ms-num whitespace-nowrap text-ms-muted">{{ formatWhen(u.last_login_at, 'never') }}</td>
                </tr>
              </tbody>
            </table>
          </div>
        </section>

        <section class="ms-panel lg:col-span-2" aria-labelledby="audit-h">
          <div class="ms-panel-head">
            <h2 id="audit-h" class="ms-h2">Recent audit</h2>
            <span class="text-xs text-ms-muted">last {{ audit.length }}</span>
          </div>
          <div v-if="sideErrors.audit" class="px-4 py-3 text-[13px] text-ms-error">{{ sideErrors.audit }}</div>
          <div v-else-if="!audit.length" class="px-4 py-6 text-sm text-ms-muted">Nothing recorded yet.</div>
          <div v-else class="overflow-x-auto">
            <table class="ms-table">
              <thead><tr><th>Time</th><th>Action</th><th>Actor</th><th>Detail</th></tr></thead>
              <tbody>
                <tr v-for="e in audit" :key="e.id">
                  <td class="ms-num whitespace-nowrap text-ms-muted">{{ formatWhen(e.created_at, '') }}</td>
                  <td class="whitespace-nowrap">{{ e.action }}</td>
                  <td class="ms-id">{{ e.actor }}</td>
                  <td class="max-w-[48ch] truncate" :title="e.detail">{{ e.detail }}</td>
                </tr>
              </tbody>
            </table>
          </div>
        </section>
      </div>
    </template>
  </div>
</template>
