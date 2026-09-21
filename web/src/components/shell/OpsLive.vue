<script setup>
import { onMounted, onUnmounted } from 'vue'
import { useOpsStore } from '../../stores/ops'
import { useWebSocket } from '../../utils/ws'

// Mounted only while someone is signed in: starts the shared poll and the
// live socket, and stops both on sign-out. Renders nothing.
const ops = useOpsStore()
useWebSocket(() => ops.nudge())
onMounted(() => ops.start())
onUnmounted(() => ops.stop())
</script>

<template><span hidden /></template>
