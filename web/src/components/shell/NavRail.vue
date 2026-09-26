<script setup>
import { computed } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import { useAuthStore } from '../../stores/auth'
import { useOpsStore } from '../../stores/ops'
import { usePlatformStore } from '../../stores/platform'
import { visibleGroups } from '../../nav'
import BrandLockup from '../BrandLockup.vue'
import Icon from '../Icon.vue'

const props = defineProps({
  collapsed: { type: Boolean, default: false },
  // The phone drawer shows the full rail whatever the desktop preference is.
  drawer: { type: Boolean, default: false },
})
const emit = defineEmits(['toggle', 'close'])

const auth = useAuthStore()
const ops = useOpsStore()
const platform = usePlatformStore()
const route = useRoute()
const groups = computed(() => visibleGroups(auth))
const narrow = computed(() => props.collapsed && !props.drawer)

function isActive(item) {
  if (item.to === '/') return route.path === '/'
  return route.path === item.to || route.path.startsWith(item.to + '/') ||
    (item.name === 'devices' && route.name === 'deviceDetail')
}

// Unacknowledged alerts on the Alerts entry: the one count worth carrying on
// every page, because it is the one that means somebody may be waiting. The
// platform's Requests entry carries the number of people waiting for an
// account: work, not an alarm, so it takes no colour (ISA-18.2).
function badgeFor(item) {
  if (item.badge === 'alerts') return ops.alerts.length || 0
  if (item.badge === 'signups') return platform.pendingSignups || 0
  return 0
}
function badgeClass(item) {
  return item.badge === 'alerts' ? 'bg-ms-error text-ms-on-primary' : 'bg-ms-well border border-ms-border-light text-ms-text'
}
function badgeLabel(item) {
  const n = badgeFor(item)
  return item.badge === 'alerts' ? `${n} unacknowledged` : `${n} waiting`
}
</script>

<template>
  <nav class="flex flex-col h-full bg-ms-bg2 border-r border-ms-border" :class="narrow ? 'w-14' : 'w-[232px]'"
    aria-label="Main">
    <div class="h-12 flex items-center shrink-0 border-b border-ms-border" :class="narrow ? 'justify-center' : 'px-4 justify-between'">
      <RouterLink :to="{ name: 'dashboard' }" aria-label="MeshSat Hub overview" class="rounded">
        <BrandLockup v-if="!narrow" />
        <img v-else src="/meshsat-mark-dark.png" alt="" class="h-6 w-auto hidden dark:block" width="40" height="24" />
        <img v-if="narrow" src="/meshsat-mark-light.png" alt="" class="h-6 w-auto dark:hidden" width="40" height="24" />
      </RouterLink>
      <button v-if="drawer" class="ms-btn-ghost -mr-2" aria-label="Close menu" @click="emit('close')">
        <Icon name="close" />
      </button>
    </div>

    <div class="flex-1 overflow-y-auto tactical-scroll py-2" :class="narrow ? 'px-2' : 'px-2.5'">
      <div v-for="(g, gi) in groups" :key="g.label" :class="gi ? 'mt-4' : ''">
        <div v-if="!narrow" class="px-2.5 pb-1 text-xs text-ms-muted">{{ g.label }}</div>
        <div v-else-if="gi" class="mx-2 mb-3 border-t border-ms-border" />
        <RouterLink v-for="item in g.items" :key="item.to" :to="item.to"
          class="group relative flex items-center gap-2.5 h-8 rounded-md text-[13px] transition-colors"
          :class="[
            narrow ? 'justify-center' : 'px-2.5',
            isActive(item) ? 'bg-ms-well text-ms-text font-medium' : 'text-ms-muted2 hover:text-ms-text hover:bg-ms-well/60',
          ]"
          :title="narrow ? item.label : undefined"
          :aria-current="isActive(item) ? 'page' : undefined"
          @click="emit('close')">
          <span v-if="isActive(item)" class="absolute left-0 top-1.5 bottom-1.5 w-0.5 rounded-full bg-ms-primary" aria-hidden="true" />
          <Icon :name="item.icon" :size="17" :class="isActive(item) ? 'text-ms-text' : 'text-ms-muted group-hover:text-ms-text'" />
          <span v-if="!narrow" class="truncate">{{ item.label }}</span>
          <span v-if="badgeFor(item)" class="ms-num text-[11px] font-semibold leading-none rounded-full"
            :class="[badgeClass(item), narrow ? 'absolute top-0.5 right-0.5 min-w-[16px] h-4 px-1 flex items-center justify-center' : 'ml-auto min-w-[18px] h-[18px] px-1.5 flex items-center justify-center']"
            :aria-label="badgeLabel(item)">{{ badgeFor(item) }}</span>
        </RouterLink>
      </div>
    </div>

    <div class="shrink-0 border-t border-ms-border py-2" :class="narrow ? 'px-2' : 'px-2.5'">
      <RouterLink to="/help" class="group flex items-center gap-2.5 h-8 rounded-md text-[13px] text-ms-muted2 hover:text-ms-text hover:bg-ms-well/60"
        :class="narrow ? 'justify-center' : 'px-2.5'" active-class="!bg-ms-well !text-ms-text" :title="narrow ? 'Help' : undefined" @click="emit('close')">
        <Icon name="help" :size="17" class="text-ms-muted group-hover:text-ms-text" />
        <span v-if="!narrow">Help</span>
      </RouterLink>
      <button v-if="!drawer" class="group w-full flex items-center gap-2.5 h-8 rounded-md text-[13px] text-ms-muted2 hover:text-ms-text hover:bg-ms-well/60"
        :class="narrow ? 'justify-center' : 'px-2.5'" :aria-label="narrow ? 'Expand menu' : 'Collapse menu'" :title="narrow ? 'Expand menu' : undefined"
        @click="emit('toggle')">
        <Icon :name="narrow ? 'expand' : 'collapse'" :size="17" class="text-ms-muted group-hover:text-ms-text" />
        <span v-if="!narrow">Collapse</span>
      </button>
    </div>
  </nav>
</template>
