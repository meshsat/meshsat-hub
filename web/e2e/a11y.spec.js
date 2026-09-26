// @ts-check
import { test, expect } from '@playwright/test'
import AxeBuilder from '@axe-core/playwright'

// Brand acceptance (MESHSAT-923): Login and Dashboard must have no colour-
// contrast violations in either theme. Runs against E2E_BASE_URL like the
// other specs; the dashboard part needs E2E_AUTH_TOKEN.
const AUTH_TOKEN = process.env.E2E_AUTH_TOKEN || 'meshsat-hub-nl-token'

// The platform pages exist only for a platform admin. Ask the Hub who the
// token is rather than assuming: a customer token skips them.
async function whoAmI(request) {
  const res = await request.get('/api/auth/me', { headers: { Authorization: `Bearer ${AUTH_TOKEN}` } })
  if (!res.ok()) return null
  return res.json()
}

const PLATFORM_PAGES = [
  { path: '/#/platform/requests', h1: 'Account requests' },
  { path: '/#/platform/tenants', h1: 'Tenants' },
  { path: '/#/platform/tenants/default', h1: '' },
  { path: '/#/platform/billing', h1: 'Billing' },
  { path: '/#/platform/system', h1: 'Platform' },
]

async function contrastViolations(page) {
  const results = await new AxeBuilder({ page }).withRules(['color-contrast']).analyze()
  return results.violations.flatMap((v) => v.nodes.map((n) => `${v.id}: ${n.html.slice(0, 120)} -- ${n.failureSummary?.split('\n')[1] || ''}`))
}

for (const theme of ['dark', 'light']) {
  test.describe(`a11y contrast (${theme})`, () => {
    test.beforeEach(async ({ page }) => {
      await page.addInitScript((t) => localStorage.setItem('theme', t), theme)
    })

    test('login page', async ({ page }) => {
      await page.goto('/#/login')
      await expect(page.locator('h1')).toBeVisible()
      // Reveal every login control so they are all checked.
      const other = page.getByRole('button', { name: 'Other sign-in options' })
      if (await other.isVisible().catch(() => false)) await other.click()
      await page.screenshot({ path: `test-results/login-${theme}.png`, fullPage: true })
      expect(await contrastViolations(page)).toEqual([])
    })

    test('dashboard', async ({ page }) => {
      await page.addInitScript((token) => {
        localStorage.setItem('auth_token', token)
        localStorage.setItem('auth_user', JSON.stringify({ id: 'token-user', name: 'API Token', roles: ['admin'], tenant_id: 'default' }))
      }, AUTH_TOKEN)
      await page.goto('/#/')
      await expect(page.locator('h1:has-text("Overview")')).toBeVisible({ timeout: 10000 })
      await page.waitForTimeout(500)
      await page.screenshot({ path: `test-results/dashboard-${theme}.png`, fullPage: true })
      expect(await contrastViolations(page)).toEqual([])
    })

    test('fleet', async ({ page }) => {
      await page.addInitScript((token) => {
        localStorage.setItem('auth_token', token)
        localStorage.setItem('auth_user', JSON.stringify({ id: 'token-user', name: 'API Token', roles: ['admin'], tenant_id: 'default' }))
      }, AUTH_TOKEN)
      await page.goto('/#/fleet?add=1')
      await expect(page.getByRole('button', { name: 'Cancel' })).toBeVisible({ timeout: 10000 })
      await page.waitForTimeout(500)
      await page.screenshot({ path: `test-results/fleet-${theme}.png`, fullPage: true })
      expect(await contrastViolations(page)).toEqual([])
    })

    for (const p of PLATFORM_PAGES) {
      test(`platform ${p.path}`, async ({ page, request }) => {
        const me = await whoAmI(request)
        test.skip(!me?.platform_admin, 'token is not a platform admin')
        await page.addInitScript(({ token, user }) => {
          localStorage.setItem('auth_token', token)
          localStorage.setItem('auth_user', JSON.stringify(user))
        }, { token: AUTH_TOKEN, user: me })
        await page.goto(p.path)
        if (p.h1) await expect(page.locator(`h1:has-text("${p.h1}")`)).toBeVisible({ timeout: 10000 })
        else await expect(page.locator('h1')).toBeVisible({ timeout: 10000 })
        await page.waitForTimeout(800)
        const slug = p.path.replace('/#/', '').replace(/\//g, '-')
        await page.screenshot({ path: `test-results/${slug}-${theme}.png`, fullPage: true })
        expect(await contrastViolations(page)).toEqual([])
      })
    }
  })
}
