package store

// Tenant scoping, classified (MESHSAT-1149).
//
// Four features were once implemented globally on a multi-tenant platform
// (MESHSAT-1118): each was reasonable for a single-tenant Hub, nobody asked
// again, and each became a cross-tenant defect. The Store interface is where
// that question can be asked mechanically. scoping_test.go parses the
// interface and fails the build for any method that carries no tenant --
// no `tenantID` parameter, no argument struct with a TenantID field, no
// tenant as its subject -- unless it is listed here with the reason it is
// allowed to see every tenant. It does the same for the SQL: a statement on a
// table that has a tenant_id column must filter by it, or the function that
// builds it is listed in UnfilteredByDesign with a reason.
//
// Both lists are RATCHETS. An entry may be removed when the method gains a
// tenant; adding one is a decision that has to be written down, and the
// counts in scoping_test.go may fall but never rise.

// UnscopedByDesign lists every Store method that legitimately takes no tenant,
// with the reason. Keyed by method name.
var UnscopedByDesign = map[string]string{ // #nosec G101 -- method names and the reasons they see every tenant, not credentials
	// Lifecycle and plumbing: no rows involved.
	"Migrate": "schema management, no tenant rows",
	"Close":   "connection lifecycle",
	"Ping":    "health probe",

	// Platform-wide background jobs: they run on the leader over every tenant
	// and either touch no tenant data or scope every row they touch by the
	// row's own tenant_id (checked by the SQL ratchet, not exempted from it).
	"MarkStaleBridgesOffline":      "reaper over the whole fleet; per-tenant timeout resolved inside the statement",
	"ListScheduledMessages":        "scheduler drains every tenant's due messages; each Message carries its tenant",
	"ClaimScheduledMessage":        "claim by message id; the id was read from a tenant-carrying row",
	"ExpireStaleSends":             "outbox reaper, keyed on age",
	"ListExpiringAPIKeys":          "expiry notifier over every tenant; each APIKey carries its tenant",
	"TouchAPIKeyLastUsed":          "keyed by the key's own id after the key authenticated the request",
	"ListExpiringCredentials":      "expiry notifier; each Credential carries its tenant",
	"ListDeadmanConfigs":           "the dead man's switch scan; each config carries its tenant",
	"ListBridgesWithCredentials":   "renders the NATS auth file for the whole broker; every bridge of every tenant must be in it",
	"ListTAKInstances":             "the TAK operator reconciles every tenant's instance; each carries its tenant",
	"ListOOBPeersByPeerID":         "OOB pairing lookup by the peer's wire id, which is unique across the platform",
	"ListHubCredentialsByProvider": "the pools enumerate every tenant's accounts for one provider; each Credential carries its tenant",

	// Cross-replica claims and platform config: not tenant data.
	"ClaimOnce":                 "idempotency key; callers put the tenant in the key",
	"PurgeClaims":               "claim table reaper",
	"GetSystemConfig":           "platform key/value (hub identity, cursors)",
	"SetSystemConfig":           "platform key/value",
	"ListSystemConfigOlderThan": "platform key/value reaper",
	"DeleteSystemConfig":        "platform key/value",

	// Authentication before a tenant is known.
	"GetRefreshToken":         "the token is the credential; the tenant is on the row it returns",
	"GetAPIKeyByHash":         "the key is the credential; the tenant comes back with it (main.go apikey lookup)",
	"GetPendingInviteByEmail": "a person signing in for the first time has no tenant yet; the invite naming their address decides which one",
	"DeleteRefreshToken":      "logout by the token itself",
	"AcceptInvite":            "invite id is a one-time capability; the tenant is on the invite",
	"GetOIDCIdentity":         "lookup by issuer+subject at sign-in, before any tenant is known",

	// Billing: platform-level, one Stripe account, one gapless document series.
	// Every receipt and refund row carries its tenant; the drainers and the
	// platform-admin surfaces work the whole outbox by design.
	"GetReceipt":             "platform-admin surface, keyed by receipt id",
	"GetReceiptByKey":        "idempotency lookup by delivery key",
	"GetReceiptByPaymentRef": "refund webhook matches a charge to its receipt",
	"ListDueReceipts":        "the receipt drainer works the whole outbox",
	"SetReceiptInvoice":      "drainer bookkeeping by receipt id",
	"SetReceiptPaymentRef":   "drainer bookkeeping by delivery key",
	"MarkReceiptIssued":      "drainer bookkeeping by receipt id",
	"MarkReceiptAttempt":     "drainer bookkeeping by receipt id",
	"BlockReceipt":           "drainer bookkeeping by receipt id",
	"ClaimReceipt":           "drainer claim by receipt id",
	"ReleaseReceipt":         "drainer claim by receipt id",
	"RequeueReceipt":         "platform-admin surface",
	"ListReceiptsByStatus":   "platform-admin surface (blocked receipts)",
	"CrossBorderSalesSince":  "the EU VAT threshold meter is entity-wide by law",
	"RefundsByCountrySince":  "the EU VAT threshold meter, refunds side",
	"GetRefund":              "platform-admin surface",
	"GetRefundByReceipt":     "one refund per receipt",
	"ListDueRefunds":         "the credit-note drainer works the whole outbox",
	"ListRefundsByStatus":    "platform-admin surface",
	"SetRefundCredit":        "drainer bookkeeping by refund id",
	"MarkRefundIssued":       "drainer bookkeeping by refund id",
	"MarkRefundAttempt":      "drainer bookkeeping by refund id",
	"BlockRefund":            "drainer bookkeeping by refund id",
	"RequeueRefund":          "platform-admin surface",
	"ClaimRefund":            "drainer claim by refund id",
	"ReleaseRefund":          "drainer claim by refund id",
	"DeleteRefund":           "platform-admin withdrawal of a wrong figure",
}

// UnfilteredByDesign lists the store functions whose SQL touches a table with
// a tenant_id column without naming it, keyed by function name (both
// implementations share the entry), with the reason. A function is here
// because it works across tenants on purpose (reapers, drainers, the
// platform-admin billing surfaces), because its key is a credential or a
// unique id that was itself read from a tenant-scoped row, or because the
// filter is supplied by its callers (the query helpers).
var UnfilteredByDesign = map[string]string{ // #nosec G101 -- function names and reasons, not credentials
	// Query helpers: the WHERE clause, tenant filter included, is the caller's.
	"queryBridges":     "helper; every caller passes its own WHERE, ListBridges passes tenant_id",
	"queryCredentials": "helper; every caller passes its own WHERE, ListCredentials passes tenant_id",

	// Inserts whose column list is a package constant that names tenant_id.
	"CreateReceipt": "INSERT via receiptCols, which includes tenant_id; the row carries the tenant",
	"CreateRefund":  "INSERT via refundCols, which includes tenant_id; the row carries the tenant",

	// Keyed by a credential or a one-time capability, before any tenant is known.
	"DeleteRefreshToken":      "logout by the token hash; the token is the credential",
	"AcceptInvite":            "invite id is a one-time capability minted for one address",
	"GetPendingInviteByEmail": "first sign-in: the invite decides the tenant, not the other way round",

	// Keyed by an id that was read from a tenant-scoped row moments earlier
	// by a platform-wide background job on the leader.
	"ClaimScheduledMessage": "scheduler claim by message id from ListScheduledMessages",
	"ExpireStaleSends":      "outbox reaper keyed on age across the fleet",
	"ListExpiringAPIKeys":   "expiry notifier over every tenant; each row carries its tenant",
	"TouchAPIKeyLastUsed":   "by the key's own id after it authenticated the request",

	// Billing outbox: one Stripe account, one gapless document series, worked
	// as a whole by the drainers and the platform-admin surfaces. Every row
	// carries its tenant for export and purge.
	"GetReceipt":             "platform-admin surface by receipt id",
	"GetReceiptByKey":        "idempotency lookup by delivery key",
	"GetReceiptByPaymentRef": "refund webhook matches a charge to its receipt",
	"ListDueReceipts":        "the receipt drainer works the whole outbox",
	"SetReceiptInvoice":      "drainer bookkeeping by receipt id",
	"SetReceiptPaymentRef":   "drainer bookkeeping by delivery key",
	"MarkReceiptIssued":      "drainer bookkeeping by receipt id",
	"MarkReceiptAttempt":     "drainer bookkeeping by receipt id",
	"BlockReceipt":           "drainer bookkeeping by receipt id",
	"ClaimReceipt":           "drainer claim by receipt id",
	"ReleaseReceipt":         "drainer claim by receipt id",
	"RequeueReceipt":         "platform-admin surface",
	"ListReceiptsByStatus":   "platform-admin surface (blocked receipts)",
	"CrossBorderSalesSince":  "the EU VAT threshold is entity-wide by law",
	"RefundsByCountrySince":  "the EU VAT threshold, refunds side",
	"GetRefund":              "platform-admin surface",
	"GetRefundByReceipt":     "one refund per receipt",
	"ListDueRefunds":         "the credit-note drainer works the whole outbox",
	"ListRefundsByStatus":    "platform-admin surface",
	"SetRefundCredit":        "drainer bookkeeping by refund id",
	"MarkRefundIssued":       "drainer bookkeeping by refund id",
	"MarkRefundAttempt":      "drainer bookkeeping by refund id",
	"BlockRefund":            "drainer bookkeeping by refund id",
	"RequeueRefund":          "platform-admin surface",
	"ClaimRefund":            "drainer claim by refund id",
	"ReleaseRefund":          "drainer claim by refund id",
	"DeleteRefund":           "platform-admin withdrawal of a wrong figure",
}
