package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/lxmf"
	"github.com/meshsat/meshsat-hub/internal/lxmfhub"
	"github.com/meshsat/meshsat-hub/internal/lxmfhub/lxmfhubtest"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// attachmentRouter mounts the two attachment routes the way main.go does,
// so chi fills the path parameters.
func attachmentRouter(h *LXMFHandler) http.Handler {
	r := chi.NewRouter()
	r.Get("/api/lxmf/messages/{id}/attachments", h.Attachments)
	r.Get("/api/lxmf/messages/{id}/attachments/{n}", h.Attachment)
	return r
}

// A stored message's voice note and photo are listed with their media
// types and served with the right headers; another tenant's message is not
// there; a message without attachments lists none. [MESHSAT-1735]
func TestLXMFAttachments_ListAndFetch(t *testing.T) {
	ctx := context.Background()
	st := lxmfhubtest.NewMemStore(store.DefaultTenantID, "other")
	blobs := lxmfhubtest.NewMemBlobs()
	svc := lxmfhub.New(lxmfhub.Config{}, st, lxmfhubtest.NewMemAccounts(), nil)
	svc.SetBlobs(blobs)
	msg := &store.LXMFMessage{Direction: store.LXMFDirectionIn, State: store.LXMFStateReceived, Source: "s", Destination: "d", Content: "note"}
	if err := st.InsertLXMFMessage(ctx, store.DefaultTenantID, msg); err != nil {
		t.Fatal(err)
	}
	frames := bytes.Repeat([]byte{3}, 40)
	for i, a := range []lxmf.Attachment{{Kind: "image", Format: "webp", Data: []byte("RIFF")}, {Kind: "audio", Mode: 3, Data: frames}} {
		key := "lxmf-attachments/" + store.DefaultTenantID + "/" + msg.ID + "/" + string(rune('0'+i))
		if err := blobs.Put(ctx, key, a.ContentType(), a.Data); err != nil {
			t.Fatal(err)
		}
		if err := st.InsertLXMFAttachment(ctx, store.DefaultTenantID, &store.LXMFAttachment{MessageID: msg.ID, Index: i, Kind: a.Kind, Mode: a.Mode, Format: a.Format, Size: len(a.Data), ObjectKey: key}); err != nil {
			t.Fatal(err)
		}
	}
	h := NewLXMFHandler(svc, nil, "")
	router := attachmentRouter(h)
	get := func(role, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, lxmfAs(role, http.MethodGet, path, ""))
		return rec
	}

	rec := get(hubauth.RoleViewer, "/api/lxmf/messages/"+msg.ID+"/attachments")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var resp lxmfAttachmentsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.MessageID != msg.ID || len(resp.Attachments) != 2 {
		t.Fatalf("list: %+v %v", resp, err)
	}
	if a := resp.Attachments[1]; a.Kind != "audio" || a.Mode != 3 || a.ContentType != "audio/codec2" || a.Size != 40 || a.URL != "/api/lxmf/messages/"+msg.ID+"/attachments/1" {
		t.Fatalf("audio item %+v", a)
	}
	if a := resp.Attachments[0]; a.ContentType != "image/webp" || a.Format != "webp" {
		t.Fatalf("image item %+v", a)
	}
	if strings.Contains(rec.Body.String(), "object_key") {
		t.Fatal("the object key is not the client's business")
	}

	rec = get(hubauth.RoleViewer, "/api/lxmf/messages/"+msg.ID+"/attachments/1")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "audio/codec2" || rec.Header().Get("X-LXMF-Audio-Mode") != "3" ||
		rec.Header().Get("Content-Length") != "40" || !bytes.Equal(rec.Body.Bytes(), frames) {
		t.Fatalf("audio bytes: %d %v", rec.Code, rec.Header())
	}
	rec = get(hubauth.RoleViewer, "/api/lxmf/messages/"+msg.ID+"/attachments/0")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/webp" || rec.Header().Get("X-LXMF-Audio-Mode") != "" || rec.Body.String() != "RIFF" {
		t.Fatalf("image bytes: %d %v", rec.Code, rec.Header())
	}
	for _, path := range []string{"/api/lxmf/messages/" + msg.ID + "/attachments/2", "/api/lxmf/messages/" + msg.ID + "/attachments/x", "/api/lxmf/messages/nope/attachments/0", "/api/lxmf/messages/nope/attachments"} {
		if rec := get(hubauth.RoleViewer, path); rec.Code != http.StatusNotFound {
			t.Fatalf("%s: %d", path, rec.Code)
		}
	}
	// The bytes gone from the store: 404, not a half answer.
	_ = blobs.Delete(ctx, "lxmf-attachments/"+store.DefaultTenantID+"/"+msg.ID+"/0")
	if rec := get(hubauth.RoleViewer, "/api/lxmf/messages/"+msg.ID+"/attachments/0"); rec.Code != http.StatusNotFound {
		t.Fatalf("gone bytes: %d", rec.Code)
	}
	plain := &store.LXMFMessage{Direction: store.LXMFDirectionIn, State: store.LXMFStateReceived, Source: "s", Destination: "d", Content: "text"}
	_ = st.InsertLXMFMessage(ctx, store.DefaultTenantID, plain)
	if rec := get(hubauth.RoleViewer, "/api/lxmf/messages/"+plain.ID+"/attachments"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"attachments":[]`) {
		t.Fatalf("plain message: %d %s", rec.Code, rec.Body.String())
	}
}

// The send endpoint decodes the note and the photo into the request and
// answers the service's refusals with the right status. [MESHSAT-1735]
func TestLXMFSend_Attachments(t *testing.T) {
	ctx := context.Background()
	st := lxmfhubtest.NewMemStore(store.DefaultTenantID)
	svc := lxmfhub.New(lxmfhub.Config{}, st, lxmfhubtest.NewMemAccounts(), nil)
	svc.SetBlobs(lxmfhubtest.NewMemBlobs())
	if _, err := svc.EnsureIdentity(ctx, store.DefaultTenantID, "owner@example.org"); err != nil {
		t.Fatal(err)
	}
	h := NewLXMFHandler(svc, nil, "")
	dest := strings.Repeat("ab", 16)
	frames := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 300))
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.Send(rec, lxmfAs(hubauth.RoleOperator, http.MethodPost, "/api/lxmf/messages", body))
		return rec
	}
	rec := post(`{"destination":"` + dest + `","audio":{"mode":3,"data_b64":"` + frames + `"},"image":{"format":"webp","data_b64":"` + base64.StdEncoding.EncodeToString([]byte("RIFF")) + `"}}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}
	var row store.LXMFMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &row)
	if row.State != store.LXMFStateQueued || row.Content != "" {
		t.Fatalf("row %+v", row)
	}
	if list, _ := st.ListLXMFAttachments(ctx, store.DefaultTenantID, row.ID); len(list) != 2 || list[0].Kind != "image" || list[1].Size != 300 {
		t.Fatalf("filed %+v", list)
	}
	for name, tc := range map[string]struct {
		body string
		code int
	}{
		"too large":  {`{"destination":"` + dest + `","image":{"format":"webp","data_b64":"` + base64.StdEncoding.EncodeToString(make([]byte, lxmf.MaxAttachmentBytes+1)) + `"}}`, http.StatusRequestEntityTooLarge},
		"bad mode":   {`{"destination":"` + dest + `","audio":{"mode":12,"data_b64":"` + frames + `"}}`, http.StatusBadRequest},
		"bad base64": {`{"destination":"` + dest + `","audio":{"mode":3,"data_b64":"***"}}`, http.StatusBadRequest},
		"nothing":    {`{"destination":"` + dest + `"}`, http.StatusBadRequest},
	} {
		if rec := post(tc.body); rec.Code != tc.code {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
}
