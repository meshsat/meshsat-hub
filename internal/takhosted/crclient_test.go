package takhosted

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestClient points a Client at a test server. NewClientWith exists for
// exactly this, so none of these tests need a cluster.
func newTestClient(t *testing.T, h http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewClientWith(srv.Client(), srv.URL, "meshsat-tak"), srv
}

func TestListInstancesReadsEveryTenant(t *testing.T) {
	var gotPath string
	cl, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[
			{"metadata":{"name":"t-one"},"spec":{"tenantID":"one","label":"abcdefghij"},
			 "status":{"phase":"Ready","host":"h:8089","caCertPEM":"PEM-ONE"}},
			{"metadata":{"name":"t-two"},"spec":{"tenantID":"two","label":"klmnopqrst"},
			 "status":{"phase":"Provisioning"}}]}`))
	}))

	got, err := cl.ListInstances(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d instances, want 2", len(got))
	}
	// The path must address the operator's namespace and the right group.
	for _, want := range []string{"/apis/tak.meshsat.net/v1alpha1/", "/namespaces/meshsat-tak/", "/takinstances"} {
		if !strings.Contains(gotPath, want) {
			t.Errorf("path %q is missing %q", gotPath, want)
		}
	}
	// The CA certificate is the field takfront cannot resolve a tenant without.
	if got[0].Status.CACertPEM != "PEM-ONE" {
		t.Errorf("caCertPEM = %q, want PEM-ONE", got[0].Status.CACertPEM)
	}
	if got[0].Spec.TenantID != "one" || got[0].Spec.Label != "abcdefghij" {
		t.Errorf("spec did not decode: %+v", got[0].Spec)
	}
}

// 404 and 403 must be distinguishable. On this path a 403 almost always means
// the Hub's Role in meshsat-tak is missing a rule, and that Role is new and
// cross-namespace — so conflating it with "absent" would send the next person
// looking at the wrong layer entirely.
func TestAbsentAndForbiddenAreDifferentErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		check  func(error) bool
		wantTx string
	}{
		{"missing", http.StatusNotFound, IsNotFound, "not found"},
		{"forbidden", http.StatusForbidden, IsForbidden, "Role"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cl, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"kind":"Status","message":"from the api server"}`))
			}))
			_, err := cl.GetInstance(context.Background(), "tak-abcdefghij")
			if err == nil {
				t.Fatalf("HTTP %d produced no error", tc.status)
			}
			if !tc.check(err) {
				t.Errorf("error %v is not classified as %s", err, tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantTx) {
				t.Errorf("error %q does not mention %q, so it will not point at the right layer", err, tc.wantTx)
			}
		})
	}
}

// A deleted object that is already gone is not an error: the Hub deletes on
// teardown and on purge, and a retry must not fail because the first attempt
// succeeded.
func TestDeletingSomethingAlreadyGoneSucceeds(t *testing.T) {
	cl, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	if err := cl.DeleteInstance(context.Background(), "tak-abcdefghij"); err != nil {
		t.Errorf("deleting an absent instance returned %v, want nil", err)
	}
	if err := cl.DeleteCertRequest(context.Background(), "gone"); err != nil {
		t.Errorf("deleting an absent certificate request returned %v, want nil", err)
	}
}

// Writes are server-side applies, so create and update are one idempotent call.
func TestApplyInstanceIsAServerSideApply(t *testing.T) {
	var method, ctype, query string
	var body TakInstance
	cl, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, ctype, query = r.Method, r.Header.Get("Content-Type"), r.URL.RawQuery
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))

	err := cl.ApplyInstance(context.Background(), TakInstance{
		Metadata: objectMeta{Name: "tak-abcdefghij"},
		Spec:     InstanceSpec{TenantID: "one", Label: "abcdefghij", State: "Running"},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if method != http.MethodPatch {
		t.Errorf("method = %s, want PATCH (server-side apply)", method)
	}
	if ctype != "application/apply-patch+yaml" {
		t.Errorf("content type = %q, want application/apply-patch+yaml", ctype)
	}
	if !strings.Contains(query, "fieldManager="+fieldManager) {
		t.Errorf("query %q carries no fieldManager; without one the apply owns nothing", query)
	}
	// apiVersion and kind must be set, or a server-side apply is rejected.
	if body.APIVersion != crGroup+"/"+crVersion || body.Kind != "TakInstance" {
		t.Errorf("apiVersion/kind not set on the body: %q %q", body.APIVersion, body.Kind)
	}
	if body.Metadata.Namespace != "meshsat-tak" {
		t.Errorf("namespace = %q, want meshsat-tak", body.Metadata.Namespace)
	}
	// The Hub sets the spec and must never send a status.
	if body.Status.Phase != "" {
		t.Errorf("the Hub sent a status (%q); status belongs to the operator", body.Status.Phase)
	}
}

// An instance name carries a tenant id and a certificate request name carries a
// username. Neither belongs in a log line, and the comment claiming so should be
// a test.
func TestErrorsDoNotLeakTenantIdentifyingNames(t *testing.T) {
	cl, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"kind":"Status","message":"boom"}`))
	}))

	_, err := cl.GetInstance(context.Background(), "tak-acmecorp01")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "tak-acmecorp01") {
		t.Errorf("the error names the instance: %v", err)
	}
	if !strings.Contains(err.Error(), "<redacted>") {
		t.Errorf("the error does not show the name was withheld: %v", err)
	}
	// The human-readable part of the Status object should survive, or the error
	// is a wall of JSON nobody reads.
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("the API server's message was dropped: %v", err)
	}

	_, err = cl.GetCertRequest(context.Background(), "enroll-alice")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "alice") {
		t.Errorf("the error names the user: %v", err)
	}
}

// The certificate request the Hub submits must carry the purpose and username it
// claims, and no certificate: the operator issues the subject, the requester does
// not choose it.
func TestCertRequestSendsPurposeAndUsernameAndNoStatus(t *testing.T) {
	var got TakCertificateRequest
	cl, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))

	err := cl.CreateCertRequest(context.Background(), TakCertificateRequest{
		Metadata: objectMeta{Name: "hub-abcdefghij"},
		Spec: CertReqSpec{
			Label: "abcdefghij", Purpose: PurposeHub,
			Username: HubIdentity, CSRPEM: "-----BEGIN CERTIFICATE REQUEST-----\n",
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got.Spec.Purpose != PurposeHub || got.Spec.Username != HubIdentity {
		t.Errorf("spec purpose/username = %q/%q", got.Spec.Purpose, got.Spec.Username)
	}
	if got.Spec.CSRPEM == "" {
		t.Error("the CSR was not sent")
	}
	if got.Status.CertPEM != "" || got.Status.Phase != "" {
		t.Errorf("the Hub sent a status: %+v", got.Status)
	}
	if got.Kind != "TakCertificateRequest" {
		t.Errorf("kind = %q", got.Kind)
	}
}

// The namespace is the operator's, never the Hub's own. Getting this wrong would
// make every call 404 against a namespace that holds no custom resources.
func TestTheNamespaceDefaultsToTheOperatorsNotTheHubs(t *testing.T) {
	t.Setenv("HUB_TAK_NAMESPACE", "")
	if ns := TakNamespace(); ns != "meshsat-tak" {
		t.Errorf("default namespace = %q, want meshsat-tak", ns)
	}
	if ns := NewClientWith(nil, "https://example", "").Namespace(); ns != "meshsat-tak" {
		t.Errorf("empty namespace did not fall back to meshsat-tak, got %q", ns)
	}
	t.Setenv("HUB_TAK_NAMESPACE", "meshsat-tak-staging")
	if ns := TakNamespace(); ns != "meshsat-tak-staging" {
		t.Errorf("override ignored, got %q", ns)
	}
}
