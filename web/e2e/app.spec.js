// @ts-check
import { test, expect } from '@playwright/test'

// E2E tests run against the live production deployment at hub.meshsat.net.
// Auth token is provided via E2E_AUTH_TOKEN env var or defaults to the NL site token.
const AUTH_TOKEN = process.env.E2E_AUTH_TOKEN || 'meshsat-hub-nl-token'

test.describe('Public endpoints', () => {
  test('healthz returns ok', async ({ request }) => {
    const res = await request.get('/healthz')
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(body.status).toBe('ok')
  })

  test('readyz returns status', async ({ request }) => {
    const res = await request.get('/readyz')
    const body = await res.json()
    expect(body.status).toBeDefined()
    expect(body.checks).toBeDefined()
  })

  test('unauthenticated API returns 401', async ({ request }) => {
    const res = await request.get('/api/devices')
    expect(res.status()).toBe(401)
  })
})

test.describe('Login page', () => {
  test('shows email login form by default', async ({ page }) => {
    await page.goto('/#/login')
    await expect(page.locator('input[type="email"]')).toBeVisible()
    await expect(page.locator('input[type="password"]')).toBeVisible()
    await expect(page.getByRole('button', { name: 'Sign In' })).toBeVisible()
  })

  test('can toggle to API token mode', async ({ page }) => {
    await page.goto('/#/login')
    await page.getByRole('button', { name: 'API Token' }).click()
    await expect(page.locator('input[type="email"]')).not.toBeVisible()
    await expect(page.locator('#token')).toBeVisible()
  })

  test('shows error on empty email submit', async ({ page }) => {
    await page.goto('/#/login')
    await page.getByRole('button', { name: 'Sign In' }).click()
    await expect(page.locator('text=Email and password are required')).toBeVisible()
  })

  test('successful login with API token', async ({ page }) => {
    await page.goto('/#/login')
    await page.getByRole('button', { name: 'API Token' }).click()
    await page.fill('#token', AUTH_TOKEN)
    await page.getByRole('button', { name: 'Sign In' }).click()
    await expect(page.locator('h1:has-text("Dashboard")')).toBeVisible({ timeout: 10000 })
  })
})

test.describe('Authenticated navigation', () => {
  test.beforeEach(async ({ page }) => {
    // Login via UI to get proper auth state
    await page.addInitScript((token) => {
      localStorage.setItem('auth_token', token)
      localStorage.setItem('auth_user', JSON.stringify({
        id: 'token-user',
        name: 'API Token',
        roles: ['admin'],
        tenant_id: 'default',
      }))
    }, AUTH_TOKEN)
  })

  test('dashboard loads with live data', async ({ page }) => {
    await page.goto('/#/')
    await expect(page.locator('h1:has-text("Dashboard")')).toBeVisible()
    await expect(page.locator('text=Hub Status')).toBeVisible()
    // Hub status should show "ok" from live healthz
    await expect(page.locator('text=ok')).toBeVisible({ timeout: 10000 })
  })

  test('devices page loads and shows table', async ({ page }) => {
    await page.goto('/#/devices')
    await expect(page.locator('h1:has-text("Devices")')).toBeVisible()
    await expect(page.locator('input[placeholder="IMEI"]')).toBeVisible()
    // Table header should be visible
    await expect(page.locator('th:has-text("IMEI")')).toBeVisible()
  })

  test('device CRUD flow', async ({ page }) => {
    const testIMEI = `E2E${Date.now()}`
    await page.goto('/#/devices')

    // Create device
    await page.fill('input[placeholder="IMEI"]', testIMEI)
    await page.fill('input[placeholder="Label (optional)"]', 'Playwright Test')
    await page.getByRole('button', { name: 'Add' }).click()
    await expect(page.locator(`text=${testIMEI}`)).toBeVisible({ timeout: 5000 })

    // Delete device
    const row = page.locator(`tr:has-text("${testIMEI}")`)
    await row.locator('button:has-text("Delete")').click()
    page.on('dialog', dialog => dialog.accept())
    await row.locator('button:has-text("Delete")').click()
    await expect(page.locator(`text=${testIMEI}`)).not.toBeVisible({ timeout: 5000 })
  })

  test('messages page renders', async ({ page }) => {
    await page.goto('/#/messages')
    await expect(page.locator('h1:has-text("Messages")')).toBeVisible()
  })

  test('map page shows Leaflet map', async ({ page }) => {
    await page.goto('/#/map')
    await expect(page.locator('.leaflet-container')).toBeVisible({ timeout: 10000 })
  })

  test('device config page loads', async ({ page }) => {
    await page.goto('/#/device-config')
    await expect(page.locator('h1:has-text("Device Configuration")')).toBeVisible()
  })

  test('escalation page shows chains and alerts sections', async ({ page }) => {
    await page.goto('/#/escalation')
    await expect(page.locator('h1:has-text("Escalation")')).toBeVisible()
    await expect(page.getByRole('heading', { name: 'Escalation Chains' })).toBeVisible()
  })

  test('escalation chain form toggles', async ({ page }) => {
    await page.goto('/#/escalation')
    await page.getByRole('button', { name: '+ New Chain' }).click()
    await expect(page.locator('input[placeholder="Chain name"]')).toBeVisible()
    await page.getByRole('button', { name: 'Cancel' }).first().click()
    await expect(page.locator('input[placeholder="Chain name"]')).not.toBeVisible()
  })

  test('dead man switch page loads', async ({ page }) => {
    await page.goto('/#/deadman')
    await expect(page.locator('h1:has-text("Dead Man")')).toBeVisible()
  })

  test('notifications page loads', async ({ page }) => {
    await page.goto('/#/notifications')
    await expect(page.locator('h1:has-text("Notifications")')).toBeVisible()
  })

  test('webhooks page with delivery logs', async ({ page }) => {
    await page.goto('/#/webhooks')
    await expect(page.locator('h1:has-text("Outbound Webhooks")')).toBeVisible()
    await expect(page.getByRole('heading', { name: 'Delivery Logs' })).toBeVisible()
  })

  test('webhook form toggles', async ({ page }) => {
    await page.goto('/#/webhooks')
    await page.getByRole('button', { name: '+ New Webhook' }).click()
    await expect(page.locator('input[placeholder="https://example.com/hook"]')).toBeVisible()
    await page.getByRole('button', { name: 'Cancel' }).first().click()
    await expect(page.locator('input[placeholder="https://example.com/hook"]')).not.toBeVisible()
  })

  test('OTA page loads', async ({ page }) => {
    await page.goto('/#/ota')
    await expect(page.locator('h1:has-text("OTA Updates")')).toBeVisible()
  })

  test('network page shows constellations and MPTCP', async ({ page }) => {
    await page.goto('/#/network')
    await expect(page.locator('h1:has-text("Network")')).toBeVisible()
    await expect(page.locator('text=Satellite Constellations')).toBeVisible()
    await expect(page.locator('text=MPTCP Concentrator')).toBeVisible()
    // Should show at least iridium backend from live API
    await expect(page.locator('text=iridium').first()).toBeVisible({ timeout: 10000 })
  })

  test('settings page shows live status and API reference', async ({ page }) => {
    await page.goto('/#/settings')
    await expect(page.locator('h1:has-text("Settings")')).toBeVisible()
    await expect(page.locator('text=Health')).toBeVisible()
    await expect(page.locator('text=API Reference')).toBeVisible()
  })

  test('API keys page loads (admin)', async ({ page }) => {
    await page.goto('/#/api-keys')
    await expect(page.locator('h1:has-text("API Keys")')).toBeVisible()
  })

  test('audit page loads (admin)', async ({ page }) => {
    await page.goto('/#/audit')
    await expect(page.locator('h1:has-text("Audit")')).toBeVisible()
  })

  test('all nav links present', async ({ page }) => {
    await page.goto('/#/')
    for (const href of ['#/devices', '#/escalation', '#/deadman', '#/notifications', '#/network', '#/webhooks', '#/ota', '#/settings']) {
      await expect(page.locator(`a[href="${href}"]`).first()).toBeAttached()
    }
  })

  test('user menu and logout', async ({ page }) => {
    await page.goto('/#/')
    await page.locator('button.rounded-full').click()
    await expect(page.locator('text=API Token')).toBeVisible()
    await page.getByRole('button', { name: 'Logout' }).click()
    await expect(page).toHaveURL(/login/)
  })
})

test.describe('API integration tests', () => {
  const headers = { 'Authorization': `Bearer ${AUTH_TOKEN}`, 'Content-Type': 'application/json' }

  test('GET /api/devices returns array', async ({ request }) => {
    const res = await request.get('/api/devices', { headers })
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(Array.isArray(body)).toBeTruthy()
  })

  test('GET /api/constellations returns backends', async ({ request }) => {
    const res = await request.get('/api/constellations', { headers })
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(body.backends).toBeDefined()
    expect(body.backends).toContain('iridium')
  })

  test('GET /api/mptcp/status returns MPTCP state', async ({ request }) => {
    const res = await request.get('/api/mptcp/status', { headers })
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(body.strategy).toBeDefined()
    expect(body.updated_at).toBeDefined()
  })

  test('GET /api/escalation/chains returns array', async ({ request }) => {
    const res = await request.get('/api/escalation/chains', { headers })
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(Array.isArray(body)).toBeTruthy()
  })

  test('GET /api/deadman returns array', async ({ request }) => {
    const res = await request.get('/api/deadman', { headers })
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(Array.isArray(body)).toBeTruthy()
  })

  test('GET /api/notifications/prefs returns array', async ({ request }) => {
    const res = await request.get('/api/notifications/prefs', { headers })
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(Array.isArray(body)).toBeTruthy()
  })

  test('GET /api/webhooks returns array', async ({ request }) => {
    const res = await request.get('/api/webhooks', { headers })
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(Array.isArray(body)).toBeTruthy()
  })

  test('GET /api/audit returns entries', async ({ request }) => {
    const res = await request.get('/api/audit?limit=10', { headers })
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(Array.isArray(body)).toBeTruthy()
  })

  test('GET /api/ratelimit returns array', async ({ request }) => {
    const res = await request.get('/api/ratelimit', { headers })
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(Array.isArray(body)).toBeTruthy()
  })

  test('GET /api/auth/me returns token user', async ({ request }) => {
    const res = await request.get('/api/auth/me', { headers })
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(body.id).toBe('token-user')
    expect(body.roles).toContain('admin')
  })

  test('security headers present on all responses', async ({ request }) => {
    const res = await request.get('/api/devices', { headers })
    expect(res.headers()['strict-transport-security']).toContain('max-age=')
    expect(res.headers()['x-content-type-options']).toBe('nosniff')
    expect(res.headers()['x-frame-options']).toBe('SAMEORIGIN')
    expect(res.headers()['content-security-policy']).toContain("default-src 'self'")
    expect(res.headers()['referrer-policy']).toBeDefined()
    expect(res.headers()['permissions-policy']).toBeDefined()
  })

  test('GET /api/routes returns array', async ({ request }) => {
    const res = await request.get('/api/routes', { headers })
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(Array.isArray(body)).toBeTruthy()
  })

  test('GET /api/codecs returns decoders', async ({ request }) => {
    const res = await request.get('/api/codecs', { headers })
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(Array.isArray(body)).toBeTruthy()
    expect(body.length).toBeGreaterThan(0)
  })

  test('GET /api/reticulum/identity returns dest hash', async ({ request }) => {
    const res = await request.get('/api/reticulum/identity', { headers })
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(body.dest_hash).toBeDefined()
    expect(body.dest_hash.length).toBe(32) // 16 bytes hex
    expect(body.public_key_hex).toBeDefined()
    expect(body.app_name).toBe('meshsat.hub')
  })

  test('GET /api/reticulum/routes returns routing table', async ({ request }) => {
    const res = await request.get('/api/reticulum/routes', { headers })
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(body.count).toBeDefined()
    expect(Array.isArray(body.routes)).toBeTruthy()
  })

  test('GET /api/tor/onion returns availability', async ({ request }) => {
    const res = await request.get('/api/tor/onion', { headers })
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(body.available).toBeDefined()
  })

  test('GET /api/backup/export returns backup', async ({ request }) => {
    const res = await request.get('/api/backup/export', { headers })
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(body.version).toBeDefined()
    expect(body.devices).toBeDefined()
  })

  test('routing rule CRUD via API', async ({ request }) => {
    // Create
    const createRes = await request.post('/api/routes', {
      headers,
      data: { name: 'e2e-test-route', source_type: '*', destination_type: 'mqtt', filter: '', enabled: true }
    })
    expect(createRes.ok()).toBeTruthy()
    const created = await createRes.json()
    expect(created.id).toBeDefined()

    // Read
    const getRes = await request.get(`/api/routes/${created.id}`, { headers })
    expect(getRes.ok()).toBeTruthy()
    const got = await getRes.json()
    expect(got.name).toBe('e2e-test-route')

    // Delete
    const delRes = await request.delete(`/api/routes/${created.id}`, { headers })
    expect(delRes.ok()).toBeTruthy()
  })

  test('webhook CRUD via API', async ({ request }) => {
    // Create
    const createRes = await request.post('/api/webhooks', {
      headers,
      data: { url: 'https://e2e-test.example.com/hook', events: ['mo'], enabled: true }
    })
    expect(createRes.ok()).toBeTruthy()
    const created = await createRes.json()
    expect(created.id).toBeDefined()

    // List
    const listRes = await request.get('/api/webhooks', { headers })
    expect(listRes.ok()).toBeTruthy()
    const list = await listRes.json()
    expect(list.some(w => w.id === created.id)).toBeTruthy()

    // Delete
    const delRes = await request.delete(`/api/webhooks/${created.id}`, { headers })
    expect(delRes.ok()).toBeTruthy()
  })
})

test.describe('Untested view interactions', () => {
  test.beforeEach(async ({ page }) => {
    await page.addInitScript((token) => {
      localStorage.setItem('auth_token', token)
      localStorage.setItem('auth_user', JSON.stringify({
        id: 'token-user',
        name: 'API Token',
        roles: ['admin'],
        tenant_id: 'default',
      }))
    }, AUTH_TOKEN)
  })

  test('messages page shows send forms', async ({ page }) => {
    await page.goto('/#/messages')
    await expect(page.locator('h1:has-text("Messages")')).toBeVisible()
    // MT send section should be visible
    await expect(page.locator('text=Send MT')).toBeVisible()
    // SMS send section should be visible
    await expect(page.locator('text=Send SMS')).toBeVisible()
  })

  test('routing page loads with routes table', async ({ page }) => {
    await page.goto('/#/routing')
    await expect(page.locator('h1:has-text("Routing")')).toBeVisible()
    // Should show route table or empty state
    await expect(page.locator('th:has-text("Name")').or(page.locator('text=No routes'))).toBeVisible({ timeout: 10000 })
  })

  test('routing page form toggles', async ({ page }) => {
    await page.goto('/#/routing')
    await page.getByRole('button', { name: '+ New Route' }).click()
    await expect(page.locator('input[placeholder="Route name"]')).toBeVisible()
    // Source and destination selects should appear
    await expect(page.locator('select').first()).toBeVisible()
    await page.getByRole('button', { name: 'Cancel' }).first().click()
    await expect(page.locator('input[placeholder="Route name"]')).not.toBeVisible()
  })

  test('routing test panel', async ({ page }) => {
    await page.goto('/#/routing')
    await page.getByRole('button', { name: 'Test' }).first().click()
    await expect(page.locator('text=Test Message Routing')).toBeVisible()
  })

  test('topology page loads with identity', async ({ page }) => {
    await page.goto('/#/topology')
    await expect(page.locator('h1:has-text("Reticulum Topology")')).toBeVisible()
    await expect(page.locator('text=Hub Identity')).toBeVisible()
    // Should show dest hash from live API
    await expect(page.locator('text=Dest Hash')).toBeVisible({ timeout: 10000 })
    await expect(page.locator('text=Known Nodes')).toBeVisible()
    await expect(page.locator('text=Routing Table')).toBeVisible()
  })

  test('cluster page loads', async ({ page }) => {
    await page.goto('/#/cluster')
    await expect(page.locator('h1:has-text("Cluster")')).toBeVisible()
  })

  test('users page loads with form toggle', async ({ page }) => {
    await page.goto('/#/users')
    await expect(page.locator('h1:has-text("Users")')).toBeVisible()
    await page.getByRole('button', { name: '+ New User' }).click()
    await expect(page.locator('input[type="email"]')).toBeVisible()
    await page.getByRole('button', { name: 'Cancel' }).first().click()
  })

  test('device config page shows version history', async ({ page }) => {
    await page.goto('/#/device-config')
    await expect(page.locator('h1:has-text("Device Configuration")')).toBeVisible()
    // Should have device selector
    await expect(page.locator('select').first()).toBeVisible()
  })

  test('audit page verify chain button', async ({ page }) => {
    await page.goto('/#/audit')
    await expect(page.locator('h1:has-text("Audit")')).toBeVisible()
    const verifyBtn = page.getByRole('button', { name: 'Verify Chain' })
    if (await verifyBtn.isVisible()) {
      await verifyBtn.click()
      // Should show verification result (valid or empty chain)
      await expect(page.locator('text=valid').or(page.locator('text=empty')).or(page.locator('text=verified'))).toBeVisible({ timeout: 10000 })
    }
  })

  test('API keys create and revoke flow', async ({ page }) => {
    await page.goto('/#/api-keys')
    await expect(page.locator('h1:has-text("API Keys")')).toBeVisible()

    // Open create form
    await page.getByRole('button', { name: '+ New Key' }).click()
    await expect(page.locator('input[placeholder="Key label"]')).toBeVisible()

    // Fill and create
    const keyLabel = `e2e-test-${Date.now()}`
    await page.fill('input[placeholder="Key label"]', keyLabel)
    await page.getByRole('button', { name: 'Create' }).click()

    // Should show the created key (shown once)
    await expect(page.locator('text=meshsat_').first()).toBeVisible({ timeout: 5000 })

    // Close modal/dismiss
    const dismissBtn = page.getByRole('button', { name: 'Done' }).or(page.getByRole('button', { name: 'Close' }))
    if (await dismissBtn.first().isVisible()) {
      await dismissBtn.first().click()
    }

    // Key should appear in list
    await expect(page.locator(`text=${keyLabel}`)).toBeVisible({ timeout: 5000 })

    // Revoke it
    const row = page.locator(`tr:has-text("${keyLabel}")`)
    await row.locator('button:has-text("Revoke")').click()
    // Confirm dialog if present
    page.on('dialog', dialog => dialog.accept())
    await row.locator('button:has-text("Revoke")').click()
    await expect(page.locator(`text=${keyLabel}`)).not.toBeVisible({ timeout: 5000 })
  })

  test('all nav links include new pages', async ({ page }) => {
    await page.goto('/#/')
    // Check that topology link is present (newly added)
    await expect(page.locator('a[href="#/topology"]').first()).toBeAttached()
    // Check routing link
    await expect(page.locator('a[href="#/routing"]').first()).toBeAttached()
    // Check users link
    await expect(page.locator('a[href="#/users"]').first()).toBeAttached()
    // Check costs link
    await expect(page.locator('a[href="#/costs"]').first()).toBeAttached()
  })

  test('costs page loads with summary and filters', async ({ page }) => {
    await page.goto('/#/costs')
    await expect(page.locator('h1:has-text("Cost Tracking")')).toBeVisible()
    // Summary cards should be visible
    await expect(page.locator('text=Total Cost')).toBeVisible()
    await expect(page.locator('text=Messages')).toBeVisible()
    // Filter controls should be present
    await expect(page.locator('select').first()).toBeVisible()
    await expect(page.locator('input[type="date"]').first()).toBeVisible()
    // Summary table should be visible (default view)
    await expect(page.locator('text=Summary by').or(page.locator('text=No cost data'))).toBeVisible({ timeout: 10000 })
  })
})

test.describe('Cost tracking API', () => {
  const headers = { 'Authorization': `Bearer ${AUTH_TOKEN}`, 'Content-Type': 'application/json' }

  test('GET /api/costs returns array', async ({ request }) => {
    const res = await request.get('/api/costs', { headers })
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(Array.isArray(body)).toBeTruthy()
  })

  test('GET /api/costs/summary returns array', async ({ request }) => {
    const res = await request.get('/api/costs/summary', { headers })
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(Array.isArray(body)).toBeTruthy()
  })

  test('GET /api/costs supports device filter', async ({ request }) => {
    const res = await request.get('/api/costs?device=nonexistent', { headers })
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(Array.isArray(body)).toBeTruthy()
    expect(body.length).toBe(0)
  })

  test('GET /api/costs/summary supports group_by', async ({ request }) => {
    const res = await request.get('/api/costs/summary?group_by=month', { headers })
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(Array.isArray(body)).toBeTruthy()
  })
})
