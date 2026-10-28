package lxmfhub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/meshsat/meshsat-hub/internal/lxmf"
	"github.com/meshsat/meshsat-hub/internal/objstore"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// The voice notes and photos a tenant's LXMF messages carry (MESHSAT-1735):
// the bytes live in the object store under one key per attachment, the
// rows (store.LXMFAttachment) say what is there, and a tenant keeps at most
// Config.AttachmentMaxBytes of them, the oldest evicted first. Both
// replicas read the same store, so an operator's note queued on one is sent
// by the leader on the other.

// Blobs is where the bytes live: the object store in production (S3Blobs),
// memory in tests. Get answers an error wrapping store.ErrNotFound for a
// key that is not there.
type Blobs interface {
	Put(ctx context.Context, key, contentType string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
}

// ErrNoBlob: the object store holds nothing under that key; it wraps
// store.ErrNotFound, which is what a Blobs in a test answers too.
var ErrNoBlob = fmt.Errorf("lxmf: attachment bytes are not in the object store: %w", store.ErrNotFound)

// errNoBlobStore: this Hub has no object store for attachments.
var errNoBlobStore = errors.New("lxmf: no object store is configured for attachments")

// defaultAttachmentMaxBytes is a tenant's store of attachments when the
// configuration names none: 64 MiB.
const defaultAttachmentMaxBytes = 64 << 20

// DefaultAttachmentPrefix is where the keys go when the configuration
// names no prefix.
const DefaultAttachmentPrefix = "lxmf-attachments"

// S3Blobs is Blobs on the Hub's object store client.
type S3Blobs struct {
	Client *objstore.Client
}

func (b S3Blobs) Put(ctx context.Context, key, contentType string, data []byte) error {
	return b.Client.Put(ctx, key, contentType, data)
}

func (b S3Blobs) Get(ctx context.Context, key string) ([]byte, error) {
	resp, err := b.Client.Get(ctx, key, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoBlob
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("get %s: status %d", key, resp.StatusCode)
	}
	// One attachment never exceeds the cap; a longer body is not ours.
	data, err := io.ReadAll(io.LimitReader(resp.Body, lxmf.MaxAttachmentBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > lxmf.MaxAttachmentBytes {
		return nil, fmt.Errorf("get %s: object larger than an attachment", key)
	}
	return data, nil
}

func (b S3Blobs) Delete(ctx context.Context, key string) error { return b.Client.Delete(ctx, key) }

// SetBlobs attaches the object store; without one, attachments are not
// kept (the text is) and a send with one is refused.
func (s *Service) SetBlobs(b Blobs) {
	s.mu.Lock()
	s.blobs = b
	s.mu.Unlock()
}

func (s *Service) blobStore() Blobs {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blobs
}

// attachmentKey is the object key of one attachment: the prefix, the
// tenant, the message and the index, so a tenant's objects share a prefix
// and a message's sit together.
func (s *Service) attachmentKey(tenantID, messageID string, index int) string {
	return s.cfg.AttachmentPrefix + "/" + tenantID + "/" + messageID + "/" + strconv.Itoa(index)
}

// fileAttachments writes a message's attachments, bytes first and then the
// row, and keeps the tenant under its cap. An attachment whose bytes do not
// land has no row: the API answers 404 for it, never a dangling key. The
// first error ends the filing; what was filed before it stays.
func (s *Service) fileAttachments(ctx context.Context, tenantID, messageID string, atts []lxmf.Attachment) error {
	if len(atts) == 0 {
		return nil
	}
	blobs := s.blobStore()
	if blobs == nil {
		s.noBlobsOnce.Do(func() {
			slog.Warn("lxmf: no object store configured, voice notes and photos are not kept (set the audit archive S3 settings)")
		})
		return errNoBlobStore
	}
	for i, a := range atts {
		key := s.attachmentKey(tenantID, messageID, i)
		if err := blobs.Put(ctx, key, a.ContentType(), a.Data); err != nil {
			return fmt.Errorf("lxmf: attachment %d not stored: %w", i, err)
		}
		row := &store.LXMFAttachment{MessageID: messageID, Index: i, Kind: a.Kind, Mode: a.Mode, Format: a.Format, Size: len(a.Data), ObjectKey: key, CreatedAt: s.now()}
		if err := s.store.InsertLXMFAttachment(ctx, tenantID, row); err != nil && !errors.Is(err, store.ErrDuplicate) {
			return fmt.Errorf("lxmf: attachment %d not recorded: %w", i, err)
		}
	}
	s.evictAttachments(ctx, tenantID)
	return nil
}

// evictAttachments drops the tenant's oldest attachments until the total
// fits the cap: the bytes go first, then the row, so a row never outlives
// its bytes by design (an object whose row failed to go is tried again next
// time, its row is still the oldest).
func (s *Service) evictAttachments(ctx context.Context, tenantID string) {
	cap := s.cfg.AttachmentMaxBytes
	total, err := s.store.LXMFAttachmentBytes(ctx, tenantID)
	if err != nil || total <= cap {
		return
	}
	blobs := s.blobStore()
	oldest, err := s.store.OldestLXMFAttachments(ctx, tenantID, 200)
	if err != nil {
		slog.Warn("lxmf: attachments not evicted", "tenant", tenantID, "error", err)
		return
	}
	removed := 0
	for _, a := range oldest {
		if total <= cap {
			break
		}
		if blobs != nil {
			if err := blobs.Delete(ctx, a.ObjectKey); err != nil {
				slog.Warn("lxmf: attachment bytes not deleted, row kept", "tenant", tenantID, "key", a.ObjectKey, "error", err)
				continue
			}
		}
		if err := s.store.DeleteLXMFAttachment(ctx, tenantID, a.MessageID, a.Index); err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.Warn("lxmf: attachment row not deleted", "tenant", tenantID, "message", a.MessageID, "error", err)
			continue
		}
		total -= int64(a.Size)
		removed++
	}
	if removed > 0 {
		slog.Info("lxmf: oldest attachments evicted", "tenant", tenantID, "removed", removed, "cap_bytes", cap)
	}
}

// loadAttachments reads a message's attachments back, bytes included, in
// the message's order, for a send.
func (s *Service) loadAttachments(ctx context.Context, tenantID, messageID string) ([]lxmf.Attachment, error) {
	rows, err := s.store.ListLXMFAttachments(ctx, tenantID, messageID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	blobs := s.blobStore()
	if blobs == nil {
		return nil, errNoBlobStore
	}
	out := make([]lxmf.Attachment, 0, len(rows))
	for _, r := range rows {
		data, err := blobs.Get(ctx, r.ObjectKey)
		if err != nil {
			return nil, fmt.Errorf("lxmf: attachment %d of %s: %w", r.Index, messageID, err)
		}
		out = append(out, lxmf.Attachment{Kind: r.Kind, Mode: r.Mode, Format: r.Format, Data: data})
	}
	return out, nil
}

// Attachments lists a message's attachments for the API; store.ErrNotFound
// when the tenant has no such message.
func (s *Service) Attachments(ctx context.Context, tenantID, messageID string) ([]store.LXMFAttachment, error) {
	if _, err := s.store.GetLXMFMessage(ctx, tenantID, messageID); err != nil {
		return nil, err
	}
	return s.store.ListLXMFAttachments(ctx, tenantID, messageID)
}

// Attachment reads one attachment and its bytes for the API; store.ErrNotFound
// when the message has none at index, ErrNoBlob when the bytes are gone.
func (s *Service) Attachment(ctx context.Context, tenantID, messageID string, index int) (*store.LXMFAttachment, []byte, error) {
	row, err := s.store.GetLXMFAttachment(ctx, tenantID, messageID, index)
	if err != nil {
		return nil, nil, err
	}
	blobs := s.blobStore()
	if blobs == nil {
		return row, nil, errNoBlobStore
	}
	data, err := blobs.Get(ctx, row.ObjectKey)
	if err != nil {
		return row, nil, err
	}
	return row, data, nil
}

// HasBlobs reports whether attachments can be kept on this Hub.
func (s *Service) HasBlobs() bool { return s.blobStore() != nil }

// attachmentsFromRequest decodes a send's note and photo, as the API hands
// them (already base64-decoded), and checks their shapes and the cap.
func attachmentsFromRequest(req SendRequest) ([]lxmf.Attachment, error) {
	var atts []lxmf.Attachment
	if req.Image != nil {
		a := *req.Image
		a.Kind, a.Format = lxmf.AttachmentImage, strings.ToLower(strings.TrimSpace(a.Format))
		atts = append(atts, a)
	}
	if req.Audio != nil {
		a := *req.Audio
		a.Kind = lxmf.AttachmentAudio
		atts = append(atts, a)
	}
	if _, err := lxmf.WithAttachments(lxmf.MapOf(), atts...); err != nil {
		return nil, err
	}
	return atts, nil
}
