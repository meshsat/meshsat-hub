<script setup>
// Help points at the documentation instead of carrying a second copy of it
// (MESHSAT-1083).
//
// What was here before was 125 lines of product prose, and where it disagreed
// with docs.meshsat.net the page was the wrong one. Three of its instructions
// had become actively harmful:
//
//   - "point your callback to https://hub.meshsat.net/api/webhook/rockblock".
//     Webhook URLs are per-tenant with the secret in the path; the global path
//     is a 404, so a customer following this page could not receive a message.
//   - "set a shared secret as HUB_ROCKBLOCK_SECRET" and "set HUB_TWILIO_* env
//     vars". Those are server environment variables a hosted customer has no
//     access to -- and the RockBLOCK shared-secret comparison never worked at
//     all, because Ground Control signs deliveries with an RS256 JWT.
//   - its one documentation link pointed at meshsat.net/docs/, which 404s. The
//     docs are served from docs.meshsat.net by a separate container.
//
// That is what a second copy costs: fixing a sentence here needs a frontend
// build and a deploy, so it loses the race with the product every time. The
// docs are 13 Hub pages and a faster pipeline, and they are PUBLIC, while this
// page is behind auth and therefore invisible to anyone evaluating MeshSat.
//
// So: links, the API reference that genuinely belongs in-app because it is
// generated from this running build, and nothing that can drift.
const DOCS = 'https://docs.meshsat.net'

const sections = [
  {
    title: 'Getting started',
    links: [
      ['Start here', '/hub/', 'What the Hub is and the first thing to do'],
      ['Accounts and plans', '/hub/accounts', 'What each plan allows, upgrading, lapsing'],
      ['Connect a bridge', '/hub/connect-a-bridge', 'Credentials, certificate, and where they go'],
      ['Devices', '/hub/devices', 'Registering them, and what counts against your plan'],
    ],
  },
  {
    title: 'Day to day',
    links: [
      ['Map and messages', '/hub/map-and-messages', 'The log, the map, and routing rules'],
      ['SOS and escalation', '/hub/sos-and-escalation', 'Who gets told, and how to test it'],
      ['TAK', '/hub/tak', 'A TAK server of your own, or point us at yours'],
      ['Your team', '/hub/team', 'Invites and what each role may do'],
    ],
  },
  {
    title: 'Connecting things up',
    links: [
      ['Provider accounts', '/hub/provider-accounts', 'Your own Cloudloop, Rock7 and Twilio — and your webhook URLs'],
      ['Hub API', '/hub/api', 'Doing all of this from a script'],
      ['API keys', '/hub/api-keys', 'Scoped keys, and how to revoke one'],
      ['Your data', '/hub/your-data', 'Export, the audit log, closing the account'],
    ],
  },
  {
    title: 'When something does not work',
    links: [
      ['Where to look first', '/hub/troubleshooting', 'A message that never arrived, a rule that did nothing, a phone that will not connect'],
    ],
  },
]
</script>

<template>
  <div class="p-4 lg:p-6 max-w-4xl mx-auto">
    <h1 class="text-2xl font-display font-bold mb-2">Help</h1>
    <p class="text-sm text-gray-400 mb-6">
      The documentation lives at
      <a :href="DOCS + '/hub/'" target="_blank" rel="noopener"
         class="text-brand-primary hover:text-brand-accent underline">docs.meshsat.net</a>.
      It is kept alongside the product rather than copied into it, so what you read there is
      current. It is also public, so you can send a link to somebody who has no account.
    </p>

    <div class="space-y-4">
      <div v-for="s in sections" :key="s.title"
           class="bg-tactical-surface rounded-lg border border-tactical-border p-4">
        <h2 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider mb-3">
          {{ s.title }}
        </h2>
        <ul class="space-y-2">
          <li v-for="[label, path, blurb] in s.links" :key="path" class="text-[12px] leading-relaxed">
            <a :href="DOCS + path" target="_blank" rel="noopener"
               class="text-brand-primary hover:text-brand-accent font-medium">{{ label }}</a>
            <span class="text-gray-500"> — {{ blurb }}</span>
          </li>
        </ul>
      </div>

      <!-- This one stays in the app on purpose: it is generated from the build
           that is running, so it is the only reference guaranteed to match the
           endpoints this Hub actually serves. -->
      <div class="bg-tactical-surface rounded-lg border border-tactical-border p-4">
        <h2 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider mb-2">
          API reference for this Hub
        </h2>
        <p class="text-[12px] text-gray-500 mb-3">
          Generated from the build you are signed in to, so it matches the endpoints this Hub
          serves rather than the latest release.
        </p>
        <div class="flex flex-wrap gap-x-6 gap-y-2 text-sm">
          <a href="/api/docs" target="_blank" rel="noopener"
             class="text-brand-primary hover:text-brand-accent">Swagger UI</a>
          <a href="/api/docs/swagger.json" target="_blank" rel="noopener"
             class="text-gray-400 hover:text-gray-300">OpenAPI JSON</a>
          <a href="/api/docs/swagger.yaml" target="_blank" rel="noopener"
             class="text-gray-400 hover:text-gray-300">OpenAPI YAML</a>
        </div>
      </div>
    </div>

    <p class="text-[11px] text-ms-muted mt-6 text-center">
      Something here wrong, or missing?
      <a href="mailto:hello@meshsat.net?subject=MeshSat%20Hub%3A%20documentation"
         class="underline hover:text-ms-text">Tell us</a>
      — a page nobody can follow is a bug.
    </p>
  </div>
</template>
