package hubmsg

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

// The shared test vectors of HeMB-NG stage 2 (docs/fixtures/v1, release fixtures-v1.1, whose
// integrity internal/hembfixtures pins). This package consumes them as the Hub's production
// code: the ids (C1e), the content digest (C1d), the Hub's ingress (C4) against a real store,
// the receipt kinds (C6a) and the reducer (C6) in every order with duplicates.

func fixture(t *testing.T, name string, v any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "v1", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func TestFixtureIDs(t *testing.T) {
	var f struct {
		Namespace string `json:"namespace"`
		Cases     []struct {
			Case    string         `json:"case"`
			Field   string         `json:"field"`
			ID      string         `json:"id"`
			Valid   bool           `json:"valid"`
			Context map[string]any `json:"context"`
		} `json:"cases"`
	}
	fixture(t, "ids.json", &f)
	if f.Namespace != Namespace {
		t.Fatalf("namespace %s, the fixtures pin %s", Namespace, f.Namespace)
	}
	for _, c := range f.Cases {
		ctx := IDContext{RunStartMs: -1}
		if v, ok := c.Context["sender"].(string); ok {
			ctx.Sender = v
		}
		if v, ok := c.Context["name"].(string); ok {
			ctx.Name = v
		}
		if v, ok := c.Context["bridge_id"].(string); ok {
			ctx.BridgeID = v
		}
		if v, ok := c.Context["run_start_ms"].(float64); ok {
			ctx.RunStartMs = int64(v)
		}
		if got := IDValid(c.ID, c.Field, ctx); got != c.Valid {
			t.Errorf("%s: IDValid(%s) = %v, want %v", c.Case, c.ID, got, c.Valid)
		}
	}
}

func TestFixtureContentDigest(t *testing.T) {
	var f struct {
		Tag     string `json:"tag"`
		Vectors []struct {
			Case        string `json:"case"`
			Mid         string `json:"mid"`
			Recipient   string `json:"recipient"`
			ContentType string `json:"content_type"`
			MidExpires  int64  `json:"mid_expires_at_ms"`
			BodyHex     string `json:"body_hex"`
			PreimageHex string `json:"preimage_hex"`
			Digest      string `json:"content_sha256"`
		} `json:"vectors"`
	}
	fixture(t, "content_digest.json", &f)
	if f.Tag != ContentTag {
		t.Fatalf("tag %q, the fixtures pin %q", ContentTag, f.Tag)
	}
	for _, v := range f.Vectors {
		body, _ := hex.DecodeString(v.BodyHex)
		pre, err := ContentPreimage(v.Mid, v.Recipient, v.ContentType, v.MidExpires, body)
		if err != nil || hex.EncodeToString(pre) != v.PreimageHex {
			t.Errorf("%s: preimage %x, %v", v.Case, pre, err)
		}
		if d, _ := ContentSHA256(v.Mid, v.Recipient, v.ContentType, v.MidExpires, body); d != v.Digest {
			t.Errorf("%s: digest %s, want %s", v.Case, d, v.Digest)
		}
	}
}

// Every Hub ingress case, step by step against a fresh store (C4).
func TestFixtureHubIngress(t *testing.T) {
	var f struct {
		Cases []struct {
			Case  string `json:"case"`
			Steps []struct {
				Submission struct {
					Tenant       string `json:"tenant"`
					Sender       string `json:"sender"`
					OpID         string `json:"op_id"`
					Mid          string `json:"mid"`
					Recipient    string `json:"recipient"`
					ContentType  string `json:"content_type"`
					MidExpiresAt int64  `json:"mid_expires_at"`
					ExpiresAt    int64  `json:"expires_at"`
					Digest       string `json:"content_sha256"`
					BodyHex      string `json:"body_hex"`
				} `json:"submission"`
				Expect Verdict `json:"expect"`
			} `json:"steps"`
		} `json:"cases"`
	}
	fixture(t, "hub_ingress_cases.json", &f)
	if len(f.Cases) != 11 {
		t.Fatalf("%d cases, the release has 11", len(f.Cases))
	}
	for _, c := range f.Cases {
		db, err := sqlite.New(filepath.Join(t.TempDir(), "hub.db"), 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		for i, step := range c.Steps {
			s := step.Submission
			body, _ := hex.DecodeString(s.BodyHex)
			sub := Submission{V: 1, OpID: s.OpID, Mid: s.Mid, To: s.Recipient, ContentType: s.ContentType,
				MidExpiresAt: s.MidExpiresAt, ExpiresAt: s.ExpiresAt, ContentSHA256: s.Digest, Body: string(body)}
			now := time.UnixMilli(s.ExpiresAt - 60_000)
			got, _, err := Submit(context.Background(), db, s.Tenant, s.Sender, sub, now, func(string) (bool, error) { return true, nil })
			if err != nil {
				t.Fatalf("%s step %d: %v", c.Case, i+1, err)
			}
			if got != step.Expect {
				t.Errorf("%s step %d: %+v, want %+v", c.Case, i+1, got, step.Expect)
			}
		}
		_ = db.Close()
	}
}

func TestFixtureReceiptKinds(t *testing.T) {
	var f struct {
		Kinds []struct{ Kind, Class, Authority string } `json:"kinds"`
	}
	fixture(t, "c6a_receipt_kinds.json", &f)
	want := map[[3]string]bool{}
	for _, k := range f.Kinds {
		want[[3]string{k.Kind, k.Class, k.Authority}] = true
	}
	if !reflect.DeepEqual(want, allowedKinds) {
		t.Fatalf("receipt kinds %v, the release has %v", allowedKinds, want)
	}
}

// Every reducer set, in every order and with every event twice (C6).
func TestFixtureReducer(t *testing.T) {
	var f struct {
		Operation       Operation         `json:"operation"`
		ProjectionOrder []string          `json:"projection_order"`
		Source          map[string]string `json:"source_by_authority"`
		Sets            []struct {
			Case   string     `json:"case"`
			Events []Event    `json:"events"`
			Expect Projection `json:"expect"`
		} `json:"sets"`
	}
	fixture(t, "c6_reducer.json", &f)
	if !reflect.DeepEqual(f.ProjectionOrder, ProjectionOrder) || !reflect.DeepEqual(f.Source, SourceOf) {
		t.Fatalf("projection order or sources differ from the release")
	}
	for _, set := range f.Sets {
		n := 0
		permute(set.Events, func(order []Event) {
			n++
			for _, evs := range [][]Event{order, append(append([]Event{}, order...), order...)} {
				got := Reduce(f.Operation, evs)
				if !reflect.DeepEqual(got, set.Expect) {
					t.Fatalf("%s, order %d: %+v, want %+v", set.Case, n, got, set.Expect)
				}
			}
		})
	}
}

func permute(evs []Event, visit func([]Event)) {
	var rec func(k int)
	a := append([]Event{}, evs...)
	rec = func(k int) {
		if k == len(a) {
			visit(append([]Event{}, a...))
			return
		}
		for i := k; i < len(a); i++ {
			a[k], a[i] = a[i], a[k]
			rec(k + 1)
			a[k], a[i] = a[i], a[k]
		}
	}
	rec(0)
}
