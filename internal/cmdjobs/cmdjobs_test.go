package cmdjobs

import (
	"context"
	"testing"
	"time"
)

// The edge proxy re-sends a POST it thinks went unanswered. With a request id,
// the second POST must be handed the job that is already running and must not
// win the right to send the command again.
func TestARetriedCommandIsHandedTheRunningJob(t *testing.T) {
	ctx := context.Background()
	s := New(NewMemoryKV())
	first := Job{RequestID: "r-1", BridgeID: "bridge-kit-a", Cmd: "mgmt_ping", Via: "sms"}

	won, job, err := s.Begin(ctx, "default", first, true, "")
	if err != nil || !won || job.State != StatePending {
		t.Fatalf("first: won=%v job=%+v err=%v", won, job, err)
	}
	// The proxy's retry, 65 s later, while the first is still waiting for the kit.
	won, running, err := s.Begin(ctx, "default", first, true, "")
	if err != nil || won {
		t.Fatalf("retry won the claim (it would have sent the command again): won=%v err=%v", won, err)
	}
	if running == nil || running.RequestID != "r-1" || running.State != StatePending {
		t.Fatalf("retry was not handed the running job: %+v", running)
	}

	if err := s.Finish(ctx, "default", job, 200, map[string]any{"status": "ok", "bearer": "sms"}, ""); err != nil {
		t.Fatal(err)
	}
	won, done, _ := s.Begin(ctx, "default", first, true, "")
	if won || done == nil || done.State != StateDone || done.HTTPStatus != 200 || len(done.Response) == 0 {
		t.Fatalf("a retry after completion should get the finished job: won=%v %+v", won, done)
	}
}

// Without a request id the same command to the same bridge over the same bearer
// inside the window is a retry; anything that differs is a new command.
func TestWithoutARequestIDTheFingerprintDecides(t *testing.T) {
	ctx := context.Background()
	s := New(NewMemoryKV())
	fp := Fingerprint("bridge-kit-a", "mgmt_ping", "sms", nil)

	won, _, err := s.Begin(ctx, "default", Job{RequestID: "auto-1", BridgeID: "bridge-kit-a", Cmd: "mgmt_ping", Via: "sms"}, false, fp)
	if err != nil || !won {
		t.Fatalf("first: %v %v", won, err)
	}
	won, running, err := s.Begin(ctx, "default", Job{RequestID: "auto-2", BridgeID: "bridge-kit-a", Cmd: "mgmt_ping", Via: "sms"}, false, fp)
	if err != nil || won || running == nil || running.RequestID != "auto-1" {
		t.Fatalf("an identical command inside the window must attach to the first: won=%v job=%+v err=%v", won, running, err)
	}
	for name, other := range map[string]string{
		"another bridge":  Fingerprint("bridge-kit-b", "mgmt_ping", "sms", nil),
		"another command": Fingerprint("bridge-kit-a", "mgmt_status", "sms", nil),
		"another bearer":  Fingerprint("bridge-kit-a", "mgmt_ping", "imt", nil),
		"other arguments": Fingerprint("bridge-kit-a", "mgmt_ping", "sms", []byte(`{"unit":"docker"}`)),
	} {
		if other == fp {
			t.Errorf("%s has the same fingerprint", name)
		}
	}
	// Another tenant's identical command is its own.
	if won, _, _ := s.Begin(ctx, "t_other", Job{RequestID: "auto-3", BridgeID: "bridge-kit-a", Cmd: "mgmt_ping", Via: "sms"}, false, fp); !won {
		t.Error("another tenant's command was treated as a retry of this one")
	}
}

func TestAJobIsOnlyVisibleToItsTenantAndBridge(t *testing.T) {
	ctx := context.Background()
	s := New(NewMemoryKV())
	_, job, _ := s.Begin(ctx, "default", Job{RequestID: "r-9", BridgeID: "bridge-kit-a", Cmd: "mgmt_ping"}, true, "")
	_ = s.Finish(ctx, "default", job, 504, nil, "no reply before the bearer timeout")

	if got, _ := s.Get(ctx, "default", "bridge-kit-a", "r-9"); got == nil || got.State != StateFailed || got.Error == "" {
		t.Fatalf("own job: %+v", got)
	}
	if got, _ := s.Get(ctx, "t_other", "bridge-kit-a", "r-9"); got != nil {
		t.Fatal("another tenant read this job")
	}
	if got, _ := s.Get(ctx, "default", "bridge-kit-b", "r-9"); got != nil {
		t.Fatal("the job was readable under another bridge")
	}
}

func TestMemoryKVExpires(t *testing.T) {
	kv := NewMemoryKV()
	ctx := context.Background()
	if ok, _ := kv.SetNX(ctx, "k", []byte("v"), 20*time.Millisecond); !ok {
		t.Fatal("first SetNX lost")
	}
	if ok, _ := kv.SetNX(ctx, "k", []byte("w"), time.Second); ok {
		t.Fatal("second SetNX won while the key was live")
	}
	time.Sleep(40 * time.Millisecond)
	if ok, _ := kv.SetNX(ctx, "k", []byte("w"), time.Second); !ok {
		t.Fatal("SetNX lost after the key expired")
	}
}
