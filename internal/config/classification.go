package config

// Who owns each setting. MeshSat Hub is a multi-tenant SaaS and the owner's
// standing ruling is that ALL tenant-owner configuration is reachable in the UI
// and not in a file: "ALL tenant owner's configuration MUST be available via the
// UX/UI and NOT via environment variables" (2026-09-13).
//
// This table is how that stays true. An audit on 2026-09-14 read all 173 HUB_*
// variables and all 165 config fields and found 26 that describe ONE tenant's
// behaviour or credentials while living in a single global value -- APRS-IS,
// the outbound email gateway, Apprise, ntfy, hawkBit, WireGuard and the
// out-of-band command policy. None of them was put there carelessly; each was a
// reasonable choice for a single-tenant Hub, and nothing ever asked the question
// again as the product became multi-tenant. That is the failure mode this table
// exists to stop: TestEverySettingIsClassified fails the build when a new field
// appears in Config without somebody deciding whose it is.
//
// A classification is a decision, not a description. Adding a field here with
// the wrong class is worse than leaving it out, because the test will then pass.

// Class says who owns a setting and therefore where it belongs.
type Class string

const (
	// ClassPlatform: the deployment's own -- database, broker, identity, TLS,
	// observability. An environment variable is the right and final home.
	ClassPlatform Class = "platform"

	// ClassCommercial: a price, a plan ceiling or a billing credential. The
	// operator's lever, never the customer's, or the tier means nothing.
	ClassCommercial Class = "commercial"

	// ClassTenantDone: tenant-owned and already reachable in the UI. The env
	// value survives as the PLATFORM's own account or as the default a
	// self-hosted single-tenant Hub relies on -- the Hub is Apache 2.0 and that
	// deployment is a real case.
	ClassTenantDone Class = "tenant-done"

	// ClassTenantProvider: tenant-owned, moving to an encrypted per-tenant row
	// in internal/integrations (MESHSAT-1121).
	ClassTenantProvider Class = "tenant-provider"

	// ClassTenantColumn: tenant-owned POLICY -- a number or a flag, nothing
	// secret -- moving to a typed column on tenants with platform bounds, the
	// shape MESHSAT-1117 established for bridge_offline_timeout.
	ClassTenantColumn Class = "tenant-column"
)

// Classification maps every Config yaml tag to its owner. Keyed by the yaml tag
// rather than the Go field name because the tag is the name an operator actually
// writes in config.yaml.
var Classification = map[string]Class{

	// --- tenant-column: Tenant-owned policy, moving to a tenants column (MESHSAT-1121). (3)

	// APRS-IS landed first (MESHSAT-1121 T1). Each tenant now connects to the
	// network under its OWN amateur licence, and a tenant with no callsign on
	// file transmits nothing rather than borrowing the operator's.
	// Apprise and ntfy landed second (T2). The alert TARGETS were always per
	// tenant and per device; only the backend that delivered to them was global,
	// and it was unset in production -- so every notification URL a customer
	// saved was accepted and delivered nowhere.
	// The PGP email gateway landed third (T3). Its keyring is per tenant now,
	// which also removed a cross-tenant defect: contacts were one process-wide
	// map keyed by bare address, so one tenant could overwrite another's key for
	// a correspondent and read mail encrypted to it.
	// Out-of-band command policy landed fourth (T4, migration v24). The frames go
	// to the tenant's own kit over a bearer the tenant is billed for, so the rate
	// and the reply timeouts are its operational choices. There is no oob_encrypt:
	// sealing is not a preference and the variable was deleted, not moved.
	// WireGuard landed fifth (T5), with the platform's own wg-easy deployed at
	// last: the feature had been "false" with no server anywhere in the estate.
	// hawkBit landed last (T6), with the platform's own server deployed. hawkBit
	// is multi-tenant itself, so per-tenant OTA is per-tenant CREDENTIALS: the
	// server decides which of its tenants the caller acts in.
	"hawkbit_enabled":  ClassTenantDone,
	"hawkbit_url":      ClassTenantDone,
	"hawkbit_username": ClassTenantDone,
	"hawkbit_password": ClassTenantDone,

	"wg_enabled":  ClassTenantDone,
	"wg_url":      ClassTenantDone,
	"wg_password": ClassTenantDone,

	"oob_max_per_hour": ClassTenantDone,
	"oob_sms_timeout":  ClassTenantDone,
	"oob_sat_timeout":  ClassTenantDone,

	"email_enabled":   ClassTenantDone,
	"email_from":      ClassTenantDone,
	"email_password":  ClassTenantDone,
	"email_pgp_key":   ClassTenantDone,
	"email_smtp_host": ClassTenantDone,
	"email_username":  ClassTenantDone,

	"apprise_enabled": ClassTenantDone,
	"apprise_url":     ClassTenantDone,
	"ntfy_enabled":    ClassTenantDone,
	"ntfy_token":      ClassTenantDone,
	"ntfy_url":        ClassTenantDone,

	"aprsis_callsign": ClassTenantDone,
	"aprsis_enabled":  ClassTenantDone,
	"aprsis_passcode": ClassTenantDone,
	"aprsis_server":   ClassTenantDone,

	// --- tenant-done: Tenant-owned and already in the UI; env is the platform default. (26)
	"audit_retention_days":      ClassTenantDone,
	"bridge_offline_timeout":    ClassTenantDone,
	"cloudloop_account_id":      ClassTenantDone,
	"cloudloop_api_key":         ClassTenantDone,
	"cloudloop_api_url":         ClassTenantDone,
	"cloudloop_webhook_token":   ClassTenantDone,
	"email_webhook_secret":      ClassTenantDone,
	"geofence_cooldown_sec":     ClassTenantDone,
	"globalstar_api_key":        ClassTenantDone,
	"globalstar_api_url":        ClassTenantDone,
	"globalstar_webhook_secret": ClassTenantDone,
	"ratelimit_burst":           ClassTenantDone,
	"ratelimit_daily_cap":       ClassTenantDone,
	"ratelimit_monthly_cap":     ClassTenantDone,
	"ratelimit_refill_per_min":  ClassTenantDone,
	"rock7_password":            ClassTenantDone,
	"rock7_username":            ClassTenantDone,
	"rockblock_secret":          ClassTenantDone,
	// Platform, deliberately, even though the secret beside it is tenant-owned:
	// this decides whether an UNSIGNED delivery is accepted on a tenant's own
	// capability path (MESHSAT-1247). A tenant who could turn it off would be
	// turning off the check that a delivery really came from Ground Control,
	// which is the platform's posture to set, not the customer's.
	"rockblock_require_signature": ClassPlatform,
	"sms_account_sid":             ClassTenantDone,
	"sms_api_key_sid":             ClassTenantDone,
	"sms_auth_token":              ClassTenantDone,
	"sms_enabled":                 ClassTenantDone,
	"sms_from_number":             ClassTenantDone,
	"whatsapp_from_number":        ClassTenantDone,
	// The platform account's Twilio ACCOUNT auth token, used only to verify
	// X-Twilio-Signature on the inbound webhook (MESHSAT-1168). Tenant-owned and
	// already in the UI: it is the Twilio provider's existing "auth_token" field
	// in internal/integrations, whose hint has always said it validates the
	// inbound signature. This entry is the platform tenant's copy of it.
	"sms_inbound_auth_token": ClassTenantDone,
	"sms_webhook_secret":     ClassTenantDone,
	// The WhatsApp bearer, on the platform's own Twilio account and WABA. Same
	// shape as sms_enabled: tenant-owned in principle, and the env value is the
	// platform account's (MESHSAT-1175).
	"whatsapp_enabled": ClassTenantDone,
	"sos_chain_id":     ClassTenantDone,

	// --- commercial: Commercial: the operator's lever. (17)
	"invoiceninja_country_id": ClassCommercial,
	"invoiceninja_currency":   ClassCommercial,
	"invoiceninja_tax_name":   ClassCommercial,
	"invoiceninja_tax_rate":   ClassCommercial,
	"invoiceninja_timeout":    ClassCommercial,
	"invoiceninja_token":      ClassCommercial,
	"invoiceninja_url":        ClassCommercial,
	"plan_device_limits":      ClassCommercial,
	"plan_send_caps":          ClassCommercial,
	"plan_tak_user_limits":    ClassCommercial,
	"stripe_donation_price":   ClassCommercial,
	"stripe_path_secret":      ClassCommercial,
	"stripe_prices":           ClassCommercial,
	"stripe_publishable_key":  ClassCommercial,
	"stripe_secret_key":       ClassCommercial,
	"stripe_timeout":          ClassCommercial,
	"stripe_webhook_secret":   ClassCommercial,

	// The four keyed by Go field name carry yaml:"-": they are S3 credentials,
	// deliberately kept out of the config FILE so they can only arrive by
	// environment. They still have an owner, so they are still classified.
	// Bounds on what a tenant owner may choose for the above. The BOUNDS are the
	// platform's: both ends are harmful, so the operator sets the range and the
	// customer picks inside it.
	"oob_max_per_hour_min": ClassPlatform,
	"oob_max_per_hour_max": ClassPlatform,
	// The ceiling on the send budget a tenant owner may give their own devices.
	"ratelimit_daily_cap_max":   ClassPlatform,
	"ratelimit_monthly_cap_max": ClassPlatform,
	"oob_timeout_min":           ClassPlatform,
	"oob_timeout_max":           ClassPlatform,

	// --- platform: The deployment's own. (98)
	"AuditArchiveS3AccessKey":        ClassPlatform,
	"AuditArchiveS3SecretKey":        ClassPlatform,
	"BasemapS3AccessKey":             ClassPlatform,
	"BasemapS3SecretKey":             ClassPlatform,
	"admin_notify_email":             ClassPlatform,
	"audit_archive_path":             ClassPlatform,
	"audit_archive_s3_bucket":        ClassPlatform,
	"audit_archive_s3_endpoint":      ClassPlatform,
	"audit_archive_s3_prefix":        ClassPlatform,
	"audit_archive_s3_region":        ClassPlatform,
	"audit_retention_max_days":       ClassPlatform,
	"audit_retention_min_days":       ClassPlatform,
	"auth_mode":                      ClassPlatform,
	"auth_rate_limit_per_min":        ClassPlatform,
	"api_rate_limit_per_min":         ClassPlatform,
	"auth_token":                     ClassPlatform,
	"authentik_token":                ClassPlatform,
	"authentik_url":                  ClassPlatform,
	"basemap_cache_max_age":          ClassPlatform,
	"basemap_s3_asset_prefix":        ClassPlatform,
	"basemap_s3_bucket":              ClassPlatform,
	"basemap_s3_endpoint":            ClassPlatform,
	"basemap_s3_key":                 ClassPlatform,
	"basemap_s3_local_key":           ClassPlatform,
	"basemap_s3_region":              ClassPlatform,
	"bridge_ca_cert_export_path":     ClassPlatform,
	"bridge_ca_secret_key":           ClassPlatform,
	"bridge_ca_secret_name":          ClassPlatform,
	"bridge_offline_timeout_max":     ClassPlatform,
	"bridge_offline_timeout_min":     ClassPlatform,
	"support_access_max_minutes":     ClassPlatform,
	"support_access_min_minutes":     ClassPlatform,
	"cloudloop_mqtt_broker":          ClassPlatform,
	"cloudloop_mqtt_ca_cert":         ClassPlatform,
	"cloudloop_mqtt_cert":            ClassPlatform,
	"cloudloop_mqtt_key":             ClassPlatform,
	"cloudloop_webhook_allowed_ips":  ClassPlatform,
	"cloudloop_webhook_expected_ips": ClassPlatform,
	"cluster_peers":                  ClassPlatform,
	"community_url":                  ClassPlatform,
	"database_url":                   ClassPlatform,
	"db_driver":                      ClassPlatform,
	"db_retry_max_attempts":          ClassPlatform,
	"db_slow_query_ms":               ClassPlatform,
	"geofence_cooldown_max":          ClassPlatform,
	"geofence_cooldown_min":          ClassPlatform,
	"health_probe_timeout":           ClassPlatform,
	"jwt_signing_key":                ClassPlatform,
	"local_login_enabled":            ClassPlatform,
	"log_format":                     ClassPlatform,
	"log_level":                      ClassPlatform,
	"mail_from":                      ClassPlatform,
	"mail_from_name":                 ClassPlatform,
	"mail_timeout":                   ClassPlatform,
	"metrics_token":                  ClassPlatform,
	"mode":                           ClassPlatform,
	"mqtt_broker_url":                ClassPlatform,
	"mqtt_client_id":                 ClassPlatform,
	"mqtt_tls_ca":                    ClassPlatform,
	"mqtt_tls_cert":                  ClassPlatform,
	"mqtt_tls_key":                   ClassPlatform,
	"nats_auth_secret_name":          ClassPlatform,
	"nats_url":                       ClassPlatform,
	"oidc_admin_group":               ClassPlatform,
	"oidc_audience":                  ClassPlatform,
	"oidc_bootstrap_owner_email":     ClassPlatform,
	"oidc_cert_pin":                  ClassPlatform,
	"oidc_cert_pin_backup":           ClassPlatform,
	"oidc_client_id":                 ClassPlatform,
	"oidc_client_secret":             ClassPlatform,
	"oidc_groups_claim":              ClassPlatform,
	"oidc_issuer_url":                ClassPlatform,
	"oidc_mfa_setup_url":             ClassPlatform,
	"oidc_password_change_url":       ClassPlatform,
	"oidc_recovery_url":              ClassPlatform,
	"oidc_redirect_uri":              ClassPlatform,
	"oidc_scopes":                    ClassPlatform,
	"oidc_signup_url":                ClassPlatform,
	"otel_endpoint":                  ClassPlatform,
	"otel_service_name":              ClassPlatform,
	"port":                           ClassPlatform,
	// The dedicated Tor listener. The deployment's own topology, not a tenant's
	// choice: it is how the Hub tells an onion request apart from an edge one
	// (MESHSAT-1169).
	"onion_port": ClassPlatform,
	// The TTC stand flow. The deployment's own: a one-off demonstration on the
	// platform's WhatsApp sender and its own kits, not a tenant feature
	// (MESHSAT-1175). booth_kits is the destination ALLOWLIST.
	"booth_enabled":           ClassPlatform,
	"booth_kits":              ClassPlatform,
	"booth_content_menu":      ClassPlatform,
	"booth_content_optin":     ClassPlatform,
	"booth_content_kits":      ClassPlatform,
	"booth_sms_keyword":       ClassPlatform,
	"booth_mesh_window":       ClassPlatform,
	"satchat_enabled":         ClassPlatform,
	"satchat_devices":         ClassPlatform,
	"satchat_max_per_hour":    ClassPlatform,
	"provision_probe_addr":    ClassPlatform,
	"pprof_enabled":           ClassPlatform,
	"public_url":              ClassPlatform,
	"redis_url":               ClassPlatform,
	"reticulum_app_name":      ClassPlatform,
	"reticulum_identity_file": ClassPlatform,
	"reticulum_tcp_addr":      ClassPlatform,
	"reticulum_tcp_enabled":   ClassPlatform,
	"shutdown_drain_seconds":  ClassPlatform,
	"smtp_relay":              ClassPlatform,
	"sqlite_path":             ClassPlatform,
	"tak_front_addr":          ClassPlatform,
	"tak_front_cert_file":     ClassPlatform,
	"tak_front_enabled":       ClassPlatform,
	"tak_front_key_file":      ClassPlatform,
	"tak_namespace":           ClassPlatform,
	"tenant_enforce":          ClassPlatform,
	"tls_pin_backup":          ClassPlatform,
	"tls_pin_primary":         ClassPlatform,
	"trusted_proxies":         ClassPlatform,
	"upgrade_url":             ClassPlatform,
}
