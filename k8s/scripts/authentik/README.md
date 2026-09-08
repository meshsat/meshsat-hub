# authentik for MeshSat Hub (MESHSAT-936)

The Hub reuses the shared authentik (namespace `omoikane` on notrf01) with its own
Brand at `auth.meshsat.net` (owner decision 2026-09-08). Everything here is applied
through `ak shell` on the worker pod; nothing touches the omoikane Brand or flows.

| File | Purpose |
|---|---|
| `run-bootstrap.sh` | wrapper: `bootstrap`, `approve <email> <role>`, `reject <email>` |
| `bootstrap-meshsat.py` | idempotent: groups, `meshsat` scope mapping, OIDC provider + application, enrollment + authentication flows, Brand `meshsat.net`, signup notification (webhook to n8n + operator email) |
| `meshsat-login.css` | `Brand.branding_custom_css` (dark MeshSat tokens, IBM Plex from meshsat.net/fonts) |
| `approve-meshsat-user.py` | activates a pending user, sets the role group, mails the activation notice; or deletes a rejected request |

## Flow

1. `Request beta access` (login page or `https://auth.meshsat.net/if/flow/meshsat-enrollment/`):
   account prompts → details prompts (organisation, country, callsign, hardware, intended use,
   Matrix ID validated as `@user:server`, "joined the MeshSat room" checkbox) → user created
   **inactive** in `meshsat-pending` → email verification → `attributes.email_verified=true`.
2. On user creation authentik fires `meshsat-new-signup`: webhook to the n8n workflow
   `NL - MeshSat Hub Signup Notifier` (Matrix `#meshsat` post + YouTrack `MESHSAT` issue
   "Signup: <email> (<organisation>)") and an email to the `meshsat-platform-admin` group.
3. Operator: `./run-bootstrap.sh approve alice@example.org owner` (or `reject`). The user is
   activated, moved from `meshsat-pending` to `meshsat-<role>`, and gets the activation email
   (Hub URL + Matrix room). The Hub refuses logins without a `meshsat-*` role group with
   `pending_approval`, so approval is the gate (MR 14).
4. First Hub login creates the tenant (JIT) with the user as owner; invites from
   Settings › Tenant join an existing tenant instead.

## Roles → Hub

`meshsat-owner|operator|viewer` map to the Hub roles; `meshsat-platform-admin` sets the
platform-admin flag (cross-tenant, `X-Tenant-ID`). Groups reach the Hub in the `groups`
claim of the `meshsat` scope (Hub requests `openid profile email meshsat`).

## Checks after `bootstrap`

- `https://auth.meshsat.net/application/o/meshsat-hub/.well-known/openid-configuration`
  reports issuer `https://auth.meshsat.net/application/o/meshsat-hub/` (Hub `HUB_OIDC_ISSUER_URL`).
- `https://auth.meshsat.net/if/flow/meshsat-enrollment/` renders the MeshSat brand;
  `https://auth.omoikane.coach/if/flow/default-authentication-flow/` is unchanged.
- OpenBao `ci-no/apps/meshsat-hub/hub` has the real `HUB_OIDC_CLIENT_ID/SECRET`; the Hub
  ExternalSecret refreshes within 1h (or `kubectl -n meshsat-hub annotate es hub-secrets force-sync=$(date +%s)`).
- Token index (2026.8.0 btree bug, omoikane `k8s/auth/NOTES.md`): `\d authentik_providers_oauth2_accesstoken` shows `USING hash (token)`.
