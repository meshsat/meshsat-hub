import { ref, onMounted, onUnmounted } from 'vue'
import { useAuthStore } from '../stores/auth'

/**
 * Composable for WebSocket real-time events from /api/ws.
 *
 * The token travels in the subprotocol, NOT in the URL: the VPS haproxy logs
 * the full request line including the query string, so `?token=` would write a
 * live access token into the edge log on all three edges (MESHSAT-1228). The
 * server echoes the "bearer" sentinel back, which is what keeps the browser
 * from aborting the upgrade.
 *
 * Reconnects with a bounded backoff, and gives up rather than hammering: a
 * socket refused for a bad or expired token would otherwise retry forever from
 * every open tab. Nothing here is load-bearing -- every view that uses this
 * still polls -- so failing quiet is the right failure.
 *
 * @param {function} onMessage - Called with parsed JSON for each event
 * @returns {{ connected: Ref<boolean> }}
 */
const RECONNECT_BASE_MS = 5000
const RECONNECT_MAX_MS = 60000
const MAX_ATTEMPTS = 6

export function useWebSocket(onMessage) {
  const connected = ref(false)
  const auth = useAuthStore()
  let ws = null
  let reconnectTimer = null
  let attempts = 0
  let stopped = false

  function connect() {
    if (stopped) return
    // No token, no socket. Opening one would be refused at the auth
    // middleware and just start the backoff for nothing.
    if (!auth.token) return

    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
    const url = `${proto}//${location.host}/api/ws`

    try {
      ws = new WebSocket(url, ['bearer', auth.token])
    } catch {
      scheduleReconnect()
      return
    }

    ws.onopen = () => {
      connected.value = true
      attempts = 0
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
      if (ws) ws.close()
    }
  }

  function scheduleReconnect() {
    if (stopped || reconnectTimer) return
    attempts += 1
    if (attempts > MAX_ATTEMPTS) {
      stopped = true
      return
    }
    const delay = Math.min(RECONNECT_BASE_MS * 2 ** (attempts - 1), RECONNECT_MAX_MS)
    reconnectTimer = setTimeout(() => {
      reconnectTimer = null
      connect()
    }, delay)
  }

  onMounted(connect)

  onUnmounted(() => {
    stopped = true
    if (reconnectTimer) clearTimeout(reconnectTimer)
    if (ws) ws.close()
  })

  return { connected }
}
