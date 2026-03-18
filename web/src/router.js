import { createRouter, createWebHashHistory } from 'vue-router'
import { useAuthStore } from './stores/auth'

const routes = [
  { path: '/login', name: 'login', component: () => import('./views/Login.vue') },
  { path: '/', name: 'dashboard', component: () => import('./views/Dashboard.vue'), meta: { requiresAuth: true } },
  { path: '/map', name: 'map', component: () => import('./views/MapView.vue'), meta: { requiresAuth: true } },
  { path: '/devices', name: 'devices', component: () => import('./views/Devices.vue'), meta: { requiresAuth: true } },
  { path: '/messages', name: 'messages', component: () => import('./views/Messages.vue'), meta: { requiresAuth: true } },
  { path: '/settings', name: 'settings', component: () => import('./views/Settings.vue'), meta: { requiresAuth: true } },
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
