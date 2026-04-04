// @ts-check
import { test, expect } from '@playwright/test'

// HeMB bond group management E2E tests — MESHSAT-490.
// Tests bond group CRUD API, HeMB stats endpoint, and Vue UI.
const AUTH_TOKEN = process.env.E2E_AUTH_TOKEN || 'meshsat-hub-nl-token'
const API_HEADERS = { 'Authorization': `Bearer ${AUTH_TOKEN}`, 'Content-Type': 'application/json' }

// Helper: create a temp bridge for bond group tests.
async function createTempBridge(request, suffix) {
  const id = `e2e-hemb-${suffix}-${Date.now()}`
  const res = await request.post('/api/bridges', {
    headers: API_HEADERS,
    data: { bridge_id: id, label: `HeMB Test ${suffix}` },
  })
  expect(res.ok()).toBeTruthy()
  return id
}

async function deleteTempBridge(request, id) {
  await request.delete(`/api/bridges/${id}`, { headers: API_HEADERS })
}

test.describe('HeMB — API integration', () => {
  test('GET /api/hemb/stats returns expected schema', async ({ request }) => {
    const res = await request.get('/api/hemb/stats', { headers: API_HEADERS })
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(body).toHaveProperty('active_streams')
    expect(body).toHaveProperty('generations_decoded')
    expect(body).toHaveProperty('generations_pending')
    expect(typeof body.active_streams).toBe('number')
  })

  test('bond group CRUD lifecycle', async ({ request }) => {
    const bridgeId = await createTempBridge(request, 'crud')

    try {
      // List: initially empty
      const listRes = await request.get(`/api/bridges/${bridgeId}/bond-groups`, { headers: API_HEADERS })
      expect(listRes.ok()).toBeTruthy()
      const empty = await listRes.json()
      expect(Array.isArray(empty)).toBeTruthy()
      expect(empty.length).toBe(0)

      // Create
      const createRes = await request.post(`/api/bridges/${bridgeId}/bond-groups`, {
        headers: API_HEADERS,
        data: { label: 'E2E Bond', members: ['mesh_0', 'iridium_0'], cost_budget: 1.50 },
      })
      expect(createRes.status()).toBe(201)
      const created = await createRes.json()
      expect(created.label).toBe('E2E Bond')
      expect(created.id).toBeDefined()
      const groupId = created.id

      // Read single
      const getRes = await request.get(`/api/bridges/${bridgeId}/bond-groups/${groupId}`, { headers: API_HEADERS })
      expect(getRes.ok()).toBeTruthy()
      const got = await getRes.json()
      expect(got.label).toBe('E2E Bond')
      expect(got.cost_budget).toBe(1.5)

      // Update
      const updateRes = await request.put(`/api/bridges/${bridgeId}/bond-groups/${groupId}`, {
        headers: API_HEADERS,
        data: { label: 'Updated Bond', members: ['mesh_0', 'sms_0'], cost_budget: 2.00 },
      })
      expect(updateRes.ok()).toBeTruthy()
      const updated = await updateRes.json()
      expect(updated.label).toBe('Updated Bond')
      expect(updated.cost_budget).toBe(2.0)

      // List: should have 1
      const listRes2 = await request.get(`/api/bridges/${bridgeId}/bond-groups`, { headers: API_HEADERS })
      const list2 = await listRes2.json()
      expect(list2.length).toBe(1)

      // Delete
      const delRes = await request.delete(`/api/bridges/${bridgeId}/bond-groups/${groupId}`, { headers: API_HEADERS })
      expect(delRes.status()).toBe(204)

      // Verify deleted
      const goneRes = await request.get(`/api/bridges/${bridgeId}/bond-groups/${groupId}`, { headers: API_HEADERS })
      expect(goneRes.status()).toBe(404)
    } finally {
      await deleteTempBridge(request, bridgeId)
    }
  })

  test('POST rejects empty label', async ({ request }) => {
    const bridgeId = await createTempBridge(request, 'val1')
    try {
      const res = await request.post(`/api/bridges/${bridgeId}/bond-groups`, {
        headers: API_HEADERS,
        data: { label: '', members: [], cost_budget: 0 },
      })
      expect(res.status()).toBe(400)
      const body = await res.json()
      expect(body.error).toContain('label')
    } finally {
      await deleteTempBridge(request, bridgeId)
    }
  })

  test('POST rejects negative cost_budget', async ({ request }) => {
    const bridgeId = await createTempBridge(request, 'val2')
    try {
      const res = await request.post(`/api/bridges/${bridgeId}/bond-groups`, {
        headers: API_HEADERS,
        data: { label: 'Bad Budget', members: [], cost_budget: -1 },
      })
      expect(res.status()).toBe(400)
      const body = await res.json()
      expect(body.error).toContain('cost_budget')
    } finally {
      await deleteTempBridge(request, bridgeId)
    }
  })

  test('bond groups are bridge-scoped (isolation)', async ({ request }) => {
    const bridgeA = await createTempBridge(request, 'isoA')
    const bridgeB = await createTempBridge(request, 'isoB')

    try {
      // Create on bridge A
      const createRes = await request.post(`/api/bridges/${bridgeA}/bond-groups`, {
        headers: API_HEADERS,
        data: { label: 'Bridge A Group', members: ['mesh_0'], cost_budget: 0 },
      })
      expect(createRes.status()).toBe(201)

      // List on bridge B should be empty
      const listB = await request.get(`/api/bridges/${bridgeB}/bond-groups`, { headers: API_HEADERS })
      const groupsB = await listB.json()
      expect(groupsB.length).toBe(0)

      // List on bridge A should have 1
      const listA = await request.get(`/api/bridges/${bridgeA}/bond-groups`, { headers: API_HEADERS })
      const groupsA = await listA.json()
      expect(groupsA.length).toBe(1)
      expect(groupsA[0].label).toBe('Bridge A Group')
    } finally {
      await deleteTempBridge(request, bridgeA)
      await deleteTempBridge(request, bridgeB)
    }
  })

  test('bond group on non-existent bridge returns 404', async ({ request }) => {
    const res = await request.post('/api/bridges/nonexistent-bridge-xyz/bond-groups', {
      headers: API_HEADERS,
      data: { label: 'Should Fail', members: [], cost_budget: 0 },
    })
    expect(res.status()).toBe(404)
  })
})

test.describe('HeMB — UI elements', () => {
  test.beforeEach(async ({ page }) => {
    await page.addInitScript((token) => {
      localStorage.setItem('auth_token', token)
      localStorage.setItem('auth_user', JSON.stringify({
        id: 'token-user', name: 'API Token', roles: ['admin'], tenant_id: 'default',
      }))
    }, AUTH_TOKEN)
  })

  test('bond groups page loads with header and bridge selector', async ({ page }) => {
    await page.goto('/#/bond-groups')
    await expect(page.locator('h1')).toContainText('Bond Groups')
    await expect(page.locator('select')).toBeVisible()
  })

  test('add form opens and cancels', async ({ page, request }) => {
    const bridgeId = await createTempBridge(request, 'ui1')
    try {
      await page.goto('/#/bond-groups')
      await page.waitForTimeout(2000)

      // Select the bridge by value (bridgeId)
      await page.locator('select').selectOption(bridgeId)
      await page.waitForTimeout(1000)

      // Open form
      await page.getByRole('button', { name: 'New Bond Group' }).click()
      await expect(page.locator('text=New Bond Group').last()).toBeVisible()

      // Cancel
      await page.getByRole('button', { name: 'Cancel' }).click()
    } finally {
      await deleteTempBridge(request, bridgeId)
    }
  })

  test('full CRUD via UI', async ({ page, request }) => {
    const bridgeId = await createTempBridge(request, 'uicrud')
    try {
      await page.goto('/#/bond-groups')
      await page.waitForTimeout(2000)

      // Select bridge by value
      await page.locator('select').selectOption(bridgeId)
      await page.waitForTimeout(1000)

      // Create
      await page.getByRole('button', { name: 'New Bond Group' }).click()
      await page.locator('input[placeholder*="SBD"]').fill('UI Test Bond')
      await page.locator('input[placeholder*="mesh_0"]').fill('mesh_0, sms_0')
      await page.locator('input[type="number"]').fill('5.00')
      await page.getByRole('button', { name: 'Create' }).click()
      await expect(page.locator('text=UI Test Bond')).toBeVisible({ timeout: 5000 })

      // Edit
      await page.locator('button[title="Edit"]').first().click()
      await expect(page.locator('text=Edit Bond Group')).toBeVisible()
      await page.locator('input[placeholder*="SBD"]').fill('Edited Bond')
      await page.getByRole('button', { name: 'Save' }).click()
      await expect(page.locator('text=Edited Bond')).toBeVisible({ timeout: 5000 })

      // Delete
      await page.locator('button[title="Delete"]').first().click()
      await expect(page.locator('text=Delete Bond Group')).toBeVisible()
      await page.getByRole('button', { name: 'Delete' }).last().click()
      await expect(page.locator('text=Edited Bond')).not.toBeVisible({ timeout: 5000 })
    } finally {
      await deleteTempBridge(request, bridgeId)
    }
  })
})
