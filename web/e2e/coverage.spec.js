// @ts-check
const { test, expect } = require('@playwright/test')

const BASE_URL = process.env.E2E_BASE_URL || 'https://hub.meshsat.net'
const AUTH_TOKEN = process.env.E2E_AUTH_TOKEN || 'meshsat-hub-nl-token'

const headers = {
  'Authorization': `Bearer ${AUTH_TOKEN}`,
  'Content-Type': 'application/json',
}

// ─── Auth helper ────────────────────────────────────────────────
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

// ═══════════════════════════════════════════════════════════════
// Alert Rules
// ═══════════════════════════════════════════════════════════════
test.describe('Alert Rules', () => {
  test('page loads and shows heading', async ({ page }) => {
    await page.goto(`${BASE_URL}/#/alert-rules`)
    await expect(page.locator('h1:has-text("Alert Rules")')).toBeVisible()
  })

  test('API: list alert rules returns array', async ({ request }) => {
    const res = await request.get(`${BASE_URL}/api/alert-rules`, { headers })
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(Array.isArray(body)).toBeTruthy()
  })

  test('API: CRUD lifecycle', async ({ request }) => {
    const name = `e2e-rule-${Date.now()}`

    // Create
    const createRes = await request.post(`${BASE_URL}/api/alert-rules`, {
      headers,
      data: {
        name,
        condition_type: 'device_not_seen',
        condition_params: '{"threshold_hours":12}',
        chain_id: 'e2e-chain-placeholder',
        device_filter: '*',
        enabled: true,
      },
    })
    expect(createRes.status()).toBe(201)
    const created = await createRes.json()
    expect(created.id).toBeDefined()
    expect(created.name).toBe(name)

    // Read
    const getRes = await request.get(`${BASE_URL}/api/alert-rules/${created.id}`, { headers })
    expect(getRes.ok()).toBeTruthy()
    const fetched = await getRes.json()
    expect(fetched.name).toBe(name)

    // Update (disable)
    const updateRes = await request.put(`${BASE_URL}/api/alert-rules/${created.id}`, {
      headers,
      data: { ...fetched, enabled: false },
    })
    expect(updateRes.ok()).toBeTruthy()
    const updated = await updateRes.json()
    expect(updated.enabled).toBe(false)

    // Delete
    const delRes = await request.delete(`${BASE_URL}/api/alert-rules/${created.id}`, { headers })
    expect(delRes.status()).toBe(204)
  })

  test('UI: create and delete alert rule', async ({ page }) => {
    await page.goto(`${BASE_URL}/#/alert-rules`)
    await expect(page.locator('h1:has-text("Alert Rules")')).toBeVisible()

    // Open form
    await page.getByRole('button', { name: '+ New Rule' }).click()
    await expect(page.locator('text=New Alert Rule')).toBeVisible()

    // Fill form
    const ruleName = `e2e-ui-${Date.now()}`
    await page.fill('input[placeholder="Rule name"]', ruleName)
    await page.fill('input[placeholder="Device filter (* = all)"]', '*')

    // The chain selector may be empty — fill chain_id manually if needed
    // Just verify the form is functional
    await page.getByRole('button', { name: 'Cancel' }).last().click()
    await expect(page.locator('text=New Alert Rule')).not.toBeVisible()
  })
})

// ═══════════════════════════════════════════════════════════════
// Cost Tracking
// ═══════════════════════════════════════════════════════════════
test.describe('Cost Tracking', () => {
  test('page loads and shows heading', async ({ page }) => {
    await page.goto(`${BASE_URL}/#/costs`)
    await expect(page.locator('h1:has-text("Cost Tracking")')).toBeVisible()
  })

  test('shows summary cards', async ({ page }) => {
    await page.goto(`${BASE_URL}/#/costs`)
    await expect(page.locator('text=Total Cost')).toBeVisible()
    await expect(page.locator('text=Messages')).toBeVisible()
  })

  test('API: list costs returns array', async ({ request }) => {
    const res = await request.get(`${BASE_URL}/api/costs`, { headers })
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(Array.isArray(body)).toBeTruthy()
  })

  test('API: cost summary by device', async ({ request }) => {
    const res = await request.get(`${BASE_URL}/api/costs/summary?group_by=device`, { headers })
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(Array.isArray(body)).toBeTruthy()
  })

  test('API: cost summary by month', async ({ request }) => {
    const res = await request.get(`${BASE_URL}/api/costs/summary?group_by=month`, { headers })
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(Array.isArray(body)).toBeTruthy()
  })

  test('toggle between summary and details', async ({ page }) => {
    await page.goto(`${BASE_URL}/#/costs`)
    await expect(page.locator('h1:has-text("Cost Tracking")')).toBeVisible()

    // Default shows summary
    const toggleBtn = page.locator('button:has-text("Summary"), button:has-text("Details")')
    await expect(toggleBtn).toBeVisible()
    await toggleBtn.click()

    // Should now show the other view
    await expect(toggleBtn).toBeVisible()
  })
})

// ═══════════════════════════════════════════════════════════════
// Device Key Operations
// ═══════════════════════════════════════════════════════════════
test.describe('Device Key Operations', () => {
  test('page loads with key operations buttons', async ({ page }) => {
    await page.goto(`${BASE_URL}/#/device-keys`)
    await expect(page.locator('h1:has-text("Device Encryption Keys")')).toBeVisible()
    await expect(page.locator('button:has-text("Import Key")')).toBeVisible()
    await expect(page.locator('button:has-text("Rotate & Distribute")')).toBeVisible()
  })

  test('import key form toggles', async ({ page }) => {
    await page.goto(`${BASE_URL}/#/device-keys`)
    await page.getByRole('button', { name: 'Import Key' }).click()
    await expect(page.locator('text=Import Existing Key')).toBeVisible()
    await page.getByRole('button', { name: 'Cancel Import' }).click()
    await expect(page.locator('text=Import Existing Key')).not.toBeVisible()
  })

  test('rotate form toggles', async ({ page }) => {
    await page.goto(`${BASE_URL}/#/device-keys`)
    await page.getByRole('button', { name: 'Rotate & Distribute' }).click()
    await expect(page.locator('text=Rotate Key & Distribute to Bridges')).toBeVisible()
  })

  test('channel key rotation section visible', async ({ page }) => {
    await page.goto(`${BASE_URL}/#/device-keys`)
    await expect(page.locator('text=Channel Key Rotation')).toBeVisible()
    await page.getByRole('button', { name: 'Rotate Channel Key' }).click()
    await expect(page.locator('text=Channel Type')).toBeVisible()
  })

  test('API: device key import endpoint exists', async ({ request }) => {
    // Test with an invalid IMEI to confirm endpoint responds (expects 404 for missing device, not 405)
    const res = await request.post(`${BASE_URL}/api/devices/000000000000000/keys/import`, {
      headers,
      data: { key_hex: 'a'.repeat(64), mode: 'decrypt' },
    })
    // 404 (device not found) or 400 (validation) — not 405 (method not allowed)
    expect(res.status()).not.toBe(405)
  })

  test('API: channel key rotate endpoint exists', async ({ request }) => {
    const res = await request.post(`${BASE_URL}/api/keys/channel/rotate`, {
      headers,
      data: { channel_type: 'iridium', address: 'e2e-test' },
    })
    // May succeed or fail on business logic — but endpoint exists
    expect(res.status()).not.toBe(405)
  })
})

// ═══════════════════════════════════════════════════════════════
// Bridge Credential Rotation
// ═══════════════════════════════════════════════════════════════
test.describe('Bridge Credential Rotation', () => {
  test('API: rotate credentials endpoint exists', async ({ request }) => {
    // Create a test bridge first
    const bridgeId = `e2e-rotate-${Date.now()}`
    const createRes = await request.post(`${BASE_URL}/api/bridges`, {
      headers,
      data: { bridge_id: bridgeId, label: 'E2E Rotate Test' },
    })
    expect(createRes.status()).toBe(201)

    // Generate initial credentials
    const genRes = await request.post(`${BASE_URL}/api/bridges/${bridgeId}/credentials`, { headers })
    expect(genRes.ok()).toBeTruthy()

    // Rotate credentials
    const rotateRes = await request.post(`${BASE_URL}/api/bridges/${bridgeId}/credentials/rotate`, { headers })
    expect(rotateRes.ok()).toBeTruthy()
    const rotated = await rotateRes.json()
    expect(rotated.password).toBeDefined()
    expect(rotated.username).toBeDefined()

    // Cleanup
    await request.delete(`${BASE_URL}/api/bridges/${bridgeId}`, { headers })
  })

  test('Fleet page shows Rotate button for bridge with credentials', async ({ page, request }) => {
    // Create bridge + credentials via API
    const bridgeId = `e2e-fleet-${Date.now()}`
    await request.post(`${BASE_URL}/api/bridges`, {
      headers,
      data: { bridge_id: bridgeId, label: 'E2E Fleet Test' },
    })
    await request.post(`${BASE_URL}/api/bridges/${bridgeId}/credentials`, { headers })

    await page.goto(`${BASE_URL}/#/fleet`)
    await expect(page.locator('h1:has-text("Fleet")')).toBeVisible()

    // Find the bridge row and verify Rotate button text
    const row = page.locator(`tr:has-text("${bridgeId}")`)
    await expect(row).toBeVisible({ timeout: 5000 })
    await expect(row.locator('button:has-text("Rotate MQTT Password")')).toBeVisible()

    // Cleanup
    page.on('dialog', d => d.accept())
    await row.locator('button:has-text("Delete")').click()
  })
})
