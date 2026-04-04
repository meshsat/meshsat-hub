// @ts-check
import { test, expect } from '@playwright/test'

// Alert Rules CRUD lifecycle tests.
// Runs against the live production deployment at hub.meshsat.net.
const AUTH_TOKEN = process.env.E2E_AUTH_TOKEN || 'meshsat-hub-nl-token'
const API_HEADERS = { 'Authorization': `Bearer ${AUTH_TOKEN}`, 'Content-Type': 'application/json' }

test.describe('Alert Rules — API CRUD', () => {
  let createdRuleId = null
  let chainId = null

  test.beforeAll(async ({ request }) => {
    // Ensure at least one escalation chain exists for the chain_id field
    const chainsRes = await request.get('/api/escalation/chains', { headers: API_HEADERS })
    const chains = await chainsRes.json()
    if (Array.isArray(chains) && chains.length > 0) {
      chainId = chains[0].id
    } else {
      // Create a temporary chain
      const createRes = await request.post('/api/escalation/chains', {
        headers: API_HEADERS,
        data: { name: `e2e-chain-${Date.now()}`, steps: [{ type: 'log', delay_sec: 0 }] },
      })
      if (createRes.ok()) {
        const chain = await createRes.json()
        chainId = chain.id
      }
    }
  })

  test('GET /api/alert-rules returns array', async ({ request }) => {
    const res = await request.get('/api/alert-rules', { headers: API_HEADERS })
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(Array.isArray(body)).toBeTruthy()
  })

  test('POST /api/alert-rules creates rule', async ({ request }) => {
    test.skip(!chainId, 'No escalation chain available')
    const ruleName = `e2e-rule-${Date.now()}`
    const res = await request.post('/api/alert-rules', {
      headers: API_HEADERS,
      data: {
        name: ruleName,
        condition_type: 'device_not_seen',
        condition_params: '{"threshold_hours":6}',
        chain_id: chainId,
        device_filter: '*',
        enabled: true,
      },
    })
    expect(res.ok()).toBeTruthy()
    const rule = await res.json()
    expect(rule.name).toBe(ruleName)
    expect(rule.id).toBeTruthy()
    expect(rule.condition_type).toBe('device_not_seen')
    expect(rule.enabled).toBe(true)
    createdRuleId = rule.id
  })

  test('GET /api/alert-rules/{id} returns created rule', async ({ request }) => {
    test.skip(!createdRuleId, 'No rule created')
    const res = await request.get(`/api/alert-rules/${createdRuleId}`, { headers: API_HEADERS })
    expect(res.ok()).toBeTruthy()
    const rule = await res.json()
    expect(rule.id).toBe(createdRuleId)
  })

  test('PUT /api/alert-rules/{id} updates rule', async ({ request }) => {
    test.skip(!createdRuleId, 'No rule created')
    const res = await request.put(`/api/alert-rules/${createdRuleId}`, {
      headers: API_HEADERS,
      data: {
        name: 'e2e-rule-updated',
        condition_type: 'battery_low',
        condition_params: '{"threshold_pct":20}',
        chain_id: chainId,
        device_filter: '*',
        enabled: false,
      },
    })
    expect(res.ok()).toBeTruthy()
    const rule = await res.json()
    expect(rule.name).toBe('e2e-rule-updated')
    expect(rule.condition_type).toBe('battery_low')
    expect(rule.enabled).toBe(false)
  })

  test('DELETE /api/alert-rules/{id} removes rule', async ({ request }) => {
    test.skip(!createdRuleId, 'No rule created')
    const res = await request.delete(`/api/alert-rules/${createdRuleId}`, { headers: API_HEADERS })
    expect(res.status()).toBe(204)

    // Verify gone
    const getRes = await request.get(`/api/alert-rules/${createdRuleId}`, { headers: API_HEADERS })
    expect(getRes.status()).toBe(404)
  })
})

test.describe('Alert Rules — UI', () => {
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

  test('alert rules page loads', async ({ page }) => {
    await page.goto('/#/alert-rules')
    await expect(page.locator('h1:has-text("Alert Rules")')).toBeVisible()
  })

  test('new rule button opens form', async ({ page }) => {
    await page.goto('/#/alert-rules')
    await expect(page.locator('h1:has-text("Alert Rules")')).toBeVisible()
    await page.getByRole('button', { name: /New Rule/i }).click()
    await expect(page.locator('input[placeholder="Rule name"]')).toBeVisible()
  })
})
