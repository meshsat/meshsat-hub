import { ref, onMounted, onUnmounted } from 'vue'

/**
 * Composable for WebSocket real-time events from /api/ws.
 * Auto-connects on mount, reconnects on close.
 * @param {function} onMessage - Called with parsed JSON for each event
 * @returns {{ connected: Ref<boolean> }}
 */
export function useWebSocket(onMessage) {
  const connected = ref(false)
  let ws = null
  let reconnectTimer = null

  function connect() {
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
    const url = `${proto}//${location.host}/api/ws`

    try {
      ws = new WebSocket(url)
    } catch {
      scheduleReconnect()
      return
    }

    ws.onopen = () => {
      connected.value = true
    }

    ws.onmessage = (event) => {
      try {
        const data = JSON.parse(event.data)
        if (onMessage) onMessage(data)
      } catch {
        // Non-JSON message, ignore
      }
    }

    ws.onclose = () => {
      connected.value = false
      scheduleReconnect()
    }

    ws.onerror = () => {
      connected.value = false
      ws.close()
    }
  }

  function scheduleReconnect() {
    if (reconnectTimer) return
    reconnectTimer = setTimeout(() => {
      reconnectTimer = null
      connect()
    }, 5000)
  }

  onMounted(connect)

  onUnmounted(() => {
    if (reconnectTimer) clearTimeout(reconnectTimer)
    if (ws) ws.close()
  })

  return { connected }
}
