package store

import (
	"encoding/base64"
	"strings"
	"unicode/utf8"
)

// RedactedInExport lists the columns a tenant export must not carry, as
// "table.column".
//
// Portability means handing someone their data, and an export is a file: it
// gets emailed, dropped in shared folders, attached to tickets. Secret
// material has a different risk profile from the records it protects, and the
// deliberate paths for reading it — the device keys API, the credentials API —
// have their own authentication and audit. So the export says what existed
// without carrying the material itself, and the manifest says so plainly.
//
// Hashes are here for the same reason as the secrets they stand for: a
// password or token hash is an offline cracking target, not portable data.
var RedactedInExport = map[string]bool{
	"api_keys.key_hash":           true,
	"bridge_oob_peers.key_enc":    true,
	"bridges.mqtt_password_hash":  true,
	"credentials.encrypted_data":  true,
	"device_keys.key_hash":        true,
	"device_keys.key_hex":         true, // the tenant's own message encryption key
	"refresh_tokens.token_hash":   true,
	"tak_users.enroll_token_hash": true,
	"tenant_invites.token_hash":   true,
	"users.password_hash":         true,
	"webhook_configs.secret":      true,
}

// RedactionMarker replaces a redacted value, so a reader can tell the
// difference between "this was empty" and "this was withheld".
const RedactionMarker = "[redacted: held back from exports, read it through its own API]"

// ExportValue prepares one column value for an export: withheld if the column
// carries secret material, decoded to text when it is text, and base64 when it
// is genuinely binary, so the archive is always valid JSON.
func ExportValue(table, column string, v any) any {
	if RedactedInExport[table+"."+column] {
		if v == nil {
			return nil
		}
		return RedactionMarker
	}
	b, ok := v.([]byte)
	if !ok {
		return v
	}
	if utf8.Valid(b) && !strings.ContainsRune(string(b), 0) {
		return string(b)
	}
	return "base64:" + base64.StdEncoding.EncodeToString(b)
}
