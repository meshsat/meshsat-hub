<script setup>
import { watch, onMounted, onUnmounted } from 'vue'
import { useOpsStore } from '../../stores/ops'
import { usePlatformStore } from '../../stores/platform'
import { useAuthStore } from '../../stores/auth'
import { useWebSocket } from '../../utils/ws'

// Mounted only while someone is signed in: starts the shared poll and the
// live socket, and stops both on sign-out. For a platform admin it also holds
// the platform store's poll (account requests waiting). Renders nothing.
const ops = useOpsStore()
const platform = usePlatformStore()
const auth = useAuthStore()
useWebSocket(() => ops.nudge())

let holdingPlatform = false
function syncPlatform(isAdmin) {
  if (isAdmin && !holdingPlatform) { platform.start(); holdingPlatform = true }
  else if (!isAdmin && holdingPlatform) { platform.stop(); holdingPlatform = false }
}

onMounted(() => { ops.start(); syncPlatform(auth.isPlatformAdmin) })
// The user record may arrive after mount (fetchUser on a fresh tab).
watch(() => auth.isPlatformAdmin, syncPlatform)
onUnmounted(() => { ops.stop(); syncPlatform(false) })
</script>

<template><span hidden /></template>
