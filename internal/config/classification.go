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

	// --- tenant-provider: Tenant-owned, still moving to internal/integrations
	// (MESHSAT-1121). This block SHRINKS as tranches land; APRS-IS left it first.
	// (13 remaining of the 22 the audit found)
	"email_enabled":    ClassTenantProvider,
	"email_from":       ClassTenantProvider,
	"email_password":   ClassTenantProvider,
	"email_pgp_key":    ClassTenantProvider,
	"email_smtp_host":  ClassTenantProvider,
	"email_username":   ClassTenantProvider,
	"hawkbit_enabled":  ClassTenantProvider,
	"hawkbit_password": ClassTenantProvider,
	"hawkbit_url":      ClassTenantProvider,
	"hawkbit_username": ClassTenantProvider,
	"wg_enabled":       ClassTenantProvider,
	"wg_password":      ClassTenantProvider,
	"wg_url":           ClassTenantProvider,

	// --- tenant-column: Tenant-owned policy, moving to a tenants column (MESHSAT-1121). (3)
	"oob_max_per_hour": ClassTenantColumn,
	"oob_sat_timeout":  ClassTenantColumn,
	"oob_sms_timeout":  ClassTenantColumn,

	// APRS-IS landed first (MESHSAT-1121 T1). Each tenant now connects to the
	// network under its OWN amateur licence, and a tenant with no callsign on
	// file transmits nothing rather than borrowing the operator's.
	// Apprise and ntfy landed second (T2). The alert TARGETS were always per
	// tenant and per device; only the backend that delivered to them was global,
	// and it was unset in production -- so every notification URL a customer
	// saved was accepted and delivered nowhere.
	"apprise_enabled": ClassTenantDone,
	"apprise_url":     ClassTenantDone,
	"ntfy_enabled":    ClassTenantDone,
	"ntfy_token":      ClassTenantDone,
	"ntfy_url":        ClassTenantDone,

	"aprsis_callsign": ClassTenantDone,
	"aprsis_enabled":  ClassTenantDone,
	"aprsis_passcode": ClassTenantDone,
	"aprsis_server":   ClassTenantDone,

	// --- tenant-done: Tenant-owned and already in the UI; env is the platform default. (25)
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
	"sms_account_sid":           ClassTenantDone,
	"sms_api_key_sid":           ClassTenantDone,
	"sms_auth_token":            ClassTenantDone,
	"sms_enabled":               ClassTenantDone,
	"sms_from_number":           ClassTenantDone,
	"sms_webhook_secret":        ClassTenantDone,
	"sos_chain_id":              ClassTenantDone,

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
	"pprof_enabled":                  ClassPlatform,
	"public_url":                     ClassPlatform,
	"redis_url":                      ClassPlatform,
	"reticulum_app_name":             ClassPlatform,
	"reticulum_identity_file":        ClassPlatform,
	"reticulum_tcp_addr":             ClassPlatform,
	"reticulum_tcp_enabled":          ClassPlatform,
	"shutdown_drain_seconds":         ClassPlatform,
	"smtp_relay":                     ClassPlatform,
	"sqlite_path":                    ClassPlatform,
	"tak_front_addr":                 ClassPlatform,
	"tak_front_cert_file":            ClassPlatform,
	"tak_front_enabled":              ClassPlatform,
	"tak_front_key_file":             ClassPlatform,
	"tak_namespace":                  ClassPlatform,
	"tenant_enforce":                 ClassPlatform,
	"tls_pin_backup":                 ClassPlatform,
	"tls_pin_primary":                ClassPlatform,
	"trusted_proxies":                ClassPlatform,
	"upgrade_url":                    ClassPlatform,
}
