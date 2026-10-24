package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// MESHSAT-1685. Who a tenant's devices may write to through the "@" lane is
// the tenant's choice: "" (the platform default, shown beside it), "off",
// "contacts" or "open". Anything else is refused, an absent field leaves the
// choice alone, and one tenant's choice never reaches another's.
func TestTheTenantChoosesWhoItsDevicesMayWriteTo(t *testing.T) {
	h, st := tsHandler(t)
	h.SetPersonChatPolicy("contacts")

	r, w := tsRequest("GET", "", tsTenant)
	h.Get(w, r)
	var got tenantResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.PersonChatMode != "" || got.PersonChatModeDefault != "contacts" {
		t.Fatalf("mode %q default %q; want the empty choice and the default beside it", got.PersonChatMode, got.PersonChatModeDefault)
	}

	r, w = tsRequest("PUT", `{"name":"Alpha","person_chat_mode":"OPEN"}`, tsTenant)
	h.Update(w, r)
	if w.Code != http.StatusOK || st.tenants[tsTenant].PersonChatMode != "open" {
		t.Fatalf("set open: %d %s / stored %q", w.Code, w.Body.String(), st.tenants[tsTenant].PersonChatMode)
	}
	if st.tenants[tsOther].PersonChatMode != "" {
		t.Fatal("one tenant's choice reached the other")
	}

	r, w = tsRequest("PUT", `{"name":"Alpha Renamed"}`, tsTenant)
	h.Update(w, r)
	if st.tenants[tsTenant].PersonChatMode != "open" {
		t.Fatal("a rename reset the mode")
	}

	r, w = tsRequest("PUT", `{"name":"Alpha","person_chat_mode":"everyone"}`, tsTenant)
	h.Update(w, r)
	if w.Code != http.StatusBadRequest || st.tenants[tsTenant].PersonChatMode != "open" {
		t.Fatalf("an unknown mode: %d / stored %q", w.Code, st.tenants[tsTenant].PersonChatMode)
	}

	r, w = tsRequest("PUT", `{"name":"Alpha","person_chat_mode":""}`, tsTenant)
	h.Update(w, r)
	if w.Code != http.StatusOK || st.tenants[tsTenant].PersonChatMode != "" {
		t.Fatalf("back to the default: %d / stored %q", w.Code, st.tenants[tsTenant].PersonChatMode)
	}
}
