import { createRouter, createWebHashHistory } from 'vue-router'

const routes = [
  { path: '/', name: 'dashboard', component: () => import('./views/Dashboard.vue') },
  { path: '/map', name: 'map', component: () => import('./views/MapView.vue') },
  { path: '/devices', name: 'devices', component: () => import('./views/Devices.vue') },
  { path: '/messages', name: 'messages', component: () => import('./views/Messages.vue') },
  { path: '/settings', name: 'settings', component: () => import('./views/Settings.vue') },
]

export default createRouter({
  history: createWebHashHistory(),
  routes,
})
