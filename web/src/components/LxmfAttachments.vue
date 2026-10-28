<script>
import { lxmf } from '../api/client'

// Module scope, shared by every message on the page: the list is read once
// per message id, and at most four reads are in flight so a hundred-row
// inbox does not flood the Hub. A failed read is forgotten so the next
// mount retries.
const lists = new Map() // message id -> Promise<[attachments]>
let inFlight = 0
const waiting = []
function slot() {
  if (inFlight < 4) { inFlight++; return Promise.resolve() }
  return new Promise(resolve => waiting.push(resolve))
}
function release() {
  const next = waiting.shift()
  if (next) next()
  else inFlight--
}

function listFor(id) {
  const key = String(id)
  if (!lists.has(key)) {
    const p = slot()
      .then(() => lxmf.attachments(key))
      .then(r => r?.attachments || [])
      .finally(release)
      .catch(err => { lists.delete(key); throw err })
    lists.set(key, p)
  }
  return lists.get(key)
}

// The attachment routes sit behind the session token like every other
// API route, so the browser cannot fetch them by URL: bytes come through
// the client and reach the players as blobs and object URLs.
async function bytesOf(id, att) {
  const blob = await lxmf.attachment(id, att.index)
  return new Uint8Array(await blob.arrayBuffer())
}
</script>

<script setup>
import { ref, watch } from 'vue'
import VoiceNote from './VoiceNote.vue'
import PhotoNote from './PhotoNote.vue'

// The voice note and photo an LXMF message carries, under its text: the
// list from GET /api/lxmf/messages/{id}/attachments (cached above), a
// photo fetched on mount, a note's bytes only when it is played. A message
// with nothing attached renders nothing. [MESHSAT-1735]
//
// The message row carries its count (attachments); a row that says 0 is
// asked for nothing, so an inbox of plain text costs no reads. Absent
// (a row from before the count) reads as unknown, and the list is fetched.
const props = defineProps({
  messageId: { type: String, required: true },
  attachments: { type: Number, default: undefined }
})

const items = ref([])
const photoOf = att => lxmf.attachment(props.messageId, att.index)
const audioOf = att => bytesOf(props.messageId, att)
watch(() => props.messageId, async id => {
  items.value = []
  if (props.attachments === 0) return
  try {
    const list = await listFor(id)
    if (props.messageId === id) items.value = list
  } catch {
    // Nothing is shown; the next mount reads again.
  }
}, { immediate: true })
</script>

<template>
  <div v-if="items.length" class="flex flex-col gap-1" data-testid="lxmf-attachments">
    <template v-for="att in items" :key="att.index">
      <PhotoNote v-if="att.kind === 'image'" :format="att.format" :size="att.size" :load="() => photoOf(att)" />
      <VoiceNote v-else-if="att.kind === 'audio'" :mode="att.mode" :size="att.size" :load="() => audioOf(att)" />
    </template>
  </div>
</template>
