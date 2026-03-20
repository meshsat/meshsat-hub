import { createRouter, createWebHashHistory } from 'vue-router'
import { useAuthStore } from './stores/auth'

const routes = [
  { path: '/login', name: 'login', component: () => import('./views/Login.vue') },
  { path: '/', name: 'dashboard', component: () => import('./views/Dashboard.vue'), meta: { requiresAuth: true } },
  { path: '/map', name: 'map', component: () => import('./views/MapView.vue'), meta: { requiresAuth: true } },
  { path: '/devices', name: 'devices', component: () => import('./views/Devices.vue'), meta: { requiresAuth: true } },
  { path: '/device-config', name: 'deviceConfig', component: () => import('./views/DeviceConfigView.vue'), meta: { requiresAuth: true } },
  { path: '/device-keys', name: 'deviceKeys', component: () => import('./views/DeviceKeysView.vue'), meta: { requiresAuth: true } },
  { path: '/messages', name: 'messages', component: () => import('./views/Messages.vue'), meta: { requiresAuth: true } },
  { path: '/escalation', name: 'escalation', component: () => import('./views/EscalationView.vue'), meta: { requiresAuth: true } },
  { path: '/deadman', name: 'deadman', component: () => import('./views/DeadmanView.vue'), meta: { requiresAuth: true } },
  { path: '/notifications', name: 'notifications', component: () => import('./views/NotificationsView.vue'), meta: { requiresAuth: true } },
  { path: '/webhooks', name: 'webhooks', component: () => import('./views/WebhooksView.vue'), meta: { requiresAuth: true } },
  { path: '/ota', name: 'ota', component: () => import('./views/OtaView.vue'), meta: { requiresAuth: true } },
  { path: '/cluster', name: 'cluster', component: () => import('./views/ClusterView.vue'), meta: { requiresAuth: true } },
  { path: '/network', name: 'network', component: () => import('./views/NetworkView.vue'), meta: { requiresAuth: true } },
  { path: '/routing', name: 'routing', component: () => import('./views/RoutingView.vue'), meta: { requiresAuth: true } },
  { path: '/topology', name: 'topology', component: () => import('./views/TopologyView.vue'), meta: { requiresAuth: true } },
  { path: '/devices/:imei', name: 'deviceDetail', component: () => import('./views/DeviceDetail.vue'), meta: { requiresAuth: true } },
  { path: '/email', name: 'email', component: () => import('./views/EmailView.vue'), meta: { requiresAuth: true } },
  { path: '/geofences', name: 'geofences', component: () => import('./views/GeofenceView.vue'), meta: { requiresAuth: true } },
  { path: '/backup', name: 'backup', component: () => import('./views/BackupView.vue'), meta: { requiresAuth: true } },
  { path: '/settings', name: 'settings', component: () => import('./views/Settings.vue'), meta: { requiresAuth: true } },
  { path: '/users', name: 'users', component: () => import('./views/UsersView.vue'), meta: { requiresAuth: true } },
  { path: '/audit', name: 'audit', component: () => import('./views/AuditView.vue'), meta: { requiresAuth: true } },
  { path: '/api-keys', name: 'apikeys', component: () => import('./views/ApiKeys.vue'), meta: { requiresAuth: true } },
]

const router = createRouter({
  history: createWebHashHistory(),
  routes,
})

router.beforeEach((to) => {
  const auth = useAuthStore()
  if (to.meta.requiresAuth && !auth.isAuthenticated) {
    return { name: 'login' }
  }
  if (to.name === 'login' && auth.isAuthenticated) {
    return { name: 'dashboard' }
  }
})

export default router
