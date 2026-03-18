// @ts-check
import { test, expect } from '@playwright/test'

// Tests hit the Vite dev server. The backend is not running, so API calls
// will fail — tests verify the UI renders correctly and handles errors gracefully.

test.describe('Login page', () => {
  test('shows login form when unauthenticated', async ({ page }) => {
    await page.goto('/#/login')
    await expect(page.locator('input[type="password"]')).toBeVisible()
    await expect(page.getByRole('button', { name: 'Sign In' })).toBeVisible()
  })

  test('shows error on empty submit', async ({ page }) => {
    await page.goto('/#/login')
    await page.getByRole('button', { name: 'Sign In' }).click()
    await expect(page.locator('text=API token is required')).toBeVisible()
  })
})

test.describe('Authenticated navigation', () => {
  test.beforeEach(async ({ page }) => {
    // Seed auth state in localStorage to bypass login
    await page.addInitScript(() => {
      localStorage.setItem('auth_token', 'test-token-e2e')
      localStorage.setItem('auth_user', JSON.stringify({
        id: 'test-user',
        name: 'E2E Tester',
        roles: ['owner'],
        tenant_id: 'test-tenant',
      }))
    })
  })

  test('renders dashboard with KPI cards', async ({ page }) => {
    await page.goto('/#/')
    await expect(page.locator('h1:has-text("Dashboard")')).toBeVisible()
    await expect(page.locator('text=Hub Status')).toBeVisible()
    await expect(page.locator('text=Iridium Credits')).toBeVisible()
  })

  test('renders devices page', async ({ page }) => {
    await page.goto('/#/devices')
    await expect(page.locator('h1:has-text("Devices")')).toBeVisible()
    await expect(page.locator('input[placeholder="IMEI"]')).toBeVisible()
  })

  test('renders messages page', async ({ page }) => {
    await page.goto('/#/messages')
    await expect(page.locator('h1:has-text("Messages")')).toBeVisible()
  })

  test('renders map page', async ({ page }) => {
    await page.goto('/#/map')
    await expect(page.locator('.leaflet-container')).toBeVisible({ timeout: 10000 })
  })

  test('renders device config page', async ({ page }) => {
    await page.goto('/#/device-config')
    await expect(page.locator('h1:has-text("Device Configuration")')).toBeVisible()
  })

  test('renders escalation page', async ({ page }) => {
    await page.goto('/#/escalation')
    await expect(page.locator('h1:has-text("Escalation")')).toBeVisible()
    await expect(page.getByRole('heading', { name: 'Escalation Chains' })).toBeVisible()
  })

  test('escalation chain form toggles', async ({ page }) => {
    await page.goto('/#/escalation')
    const btn = page.getByRole('button', { name: '+ New Chain' })
    await btn.click()
    await expect(page.locator('input[placeholder="Chain name"]')).toBeVisible()
    await page.getByRole('button', { name: 'Cancel' }).first().click()
    await expect(page.locator('input[placeholder="Chain name"]')).not.toBeVisible()
  })

  test('renders dead man switch page', async ({ page }) => {
    await page.goto('/#/deadman')
    await expect(page.locator('h1:has-text("Dead Man")')).toBeVisible()
  })

  test('renders notifications page', async ({ page }) => {
    await page.goto('/#/notifications')
    await expect(page.locator('h1:has-text("Notifications")')).toBeVisible()
  })

  test('renders webhooks page', async ({ page }) => {
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

  test('renders OTA page', async ({ page }) => {
    await page.goto('/#/ota')
    await expect(page.locator('h1:has-text("OTA Updates")')).toBeVisible()
  })

  test('OTA target form toggles', async ({ page }) => {
    await page.goto('/#/ota')
    await page.getByRole('button', { name: '+ Target' }).click()
    await expect(page.locator('input[placeholder="Controller ID (IMEI)"]')).toBeVisible()
    await page.getByRole('button', { name: 'Cancel' }).first().click()
    await expect(page.locator('input[placeholder="Controller ID (IMEI)"]')).not.toBeVisible()
  })

  test('renders network page', async ({ page }) => {
    await page.goto('/#/network')
    await expect(page.locator('h1:has-text("Network")')).toBeVisible()
    await expect(page.locator('text=Satellite Constellations')).toBeVisible()
    await expect(page.locator('text=MPTCP Concentrator')).toBeVisible()
  })

  test('renders settings page', async ({ page }) => {
    await page.goto('/#/settings')
    await expect(page.locator('h1:has-text("Settings")')).toBeVisible()
    await expect(page.locator('text=API Endpoints')).toBeVisible()
  })

  test('renders API keys page', async ({ page }) => {
    await page.goto('/#/api-keys')
    await expect(page.locator('h1:has-text("API Keys")')).toBeVisible()
  })

  test('renders audit page', async ({ page }) => {
    await page.goto('/#/audit')
    await expect(page.locator('h1:has-text("Audit")')).toBeVisible()
  })

  test('navigation links exist for all sections', async ({ page }) => {
    await page.goto('/#/')
    // Verify key nav links are present in the page
    for (const href of ['#/devices', '#/escalation', '#/deadman', '#/notifications', '#/network', '#/webhooks', '#/ota', '#/settings']) {
      await expect(page.locator(`a[href="${href}"]`).first()).toBeAttached()
    }
  })

  test('user menu shows name and role', async ({ page }) => {
    await page.goto('/#/')
    // Click user initial avatar button
    const avatar = page.locator('button.rounded-full')
    await avatar.click()
    await expect(page.locator('text=E2E Tester')).toBeVisible()
    await expect(page.locator('text=owner')).toBeVisible()
  })

  test('logout clears auth and redirects', async ({ page }) => {
    await page.goto('/#/')
    const avatar = page.locator('button.rounded-full')
    await avatar.click()
    await page.getByRole('button', { name: 'Logout' }).click()
    // Should redirect to login
    await expect(page).toHaveURL(/login/)
  })
})
