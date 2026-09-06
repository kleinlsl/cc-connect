package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testOutboxCfg() OutboxConfig {
	return OutboxConfig{
		Enabled:       true,
		MaxAge:        30 * time.Minute,
		MaxAttempts:   5,
		InitialDelay:  time.Millisecond,
		MaxDelay:      10 * time.Millisecond,
		SweepInterval: time.Hour, // tests drive sweep()/Kick() explicitly
		InitialGrace:  0,         // no grace in tests so sweep picks up immediately
	}
}

func TestOutbox_AddAndCompleteRemovesFile(t *testing.T) {
	dir := t.TempDir()
	o := NewOutbox(dir, testOutboxCfg())
	id := o.Add("feishu", "feishu:c:u", []byte(`{}`), "hello", "")
	if id == "" {
		t.Fatal("expected non-empty id")
	}
	if _, err := os.Stat(filepath.Join(dir, id+".json")); err != nil {
		t.Fatalf("expected persisted item file: %v", err)
	}
	if got := o.PendingCount(); got != 1 {
		t.Fatalf("PendingCount = %d, want 1", got)
	}

	var sent *OutboxItem
	done, err := o.deliverOnce(context.Background(), id, func(_ context.Context, it *OutboxItem) (bool, error) {
		sent = it
		return true, nil
	})
	if !done || err != nil {
		t.Fatalf("deliverOnce = %v,%v want true,nil", done, err)
	}
	if sent == nil || sent.Body != "hello" {
		t.Fatalf("sender received wrong item: %+v", sent)
	}
	if o.PendingCount() != 0 {
		t.Fatalf("PendingCount = %d after complete, want 0", o.PendingCount())
	}
	if _, err := os.Stat(filepath.Join(dir, id+".json")); !os.IsNotExist(err) {
		t.Fatalf("expected item file removed, got err=%v", err)
	}
}

// deliverOnce mirrors one sweep delivery of a single item by id.
func (o *Outbox) deliverOnce(ctx context.Context, id string, s OutboxSender) (bool, error) {
	o.mu.Lock()
	it, ok := o.items[id]
	o.mu.Unlock()
	if !ok {
		return true, nil
	}
	done, err := s(ctx, it)
	if done {
		o.Complete(id)
	} else if err != nil {
		o.Fail(id, err)
	}
	return done, err
}

func TestOutbox_RetryThenSucceed(t *testing.T) {
	dir := t.TempDir()
	o := NewOutbox(dir, testOutboxCfg())
	id := o.Add("feishu", "feishu:c:u", []byte(`{}`), "body", "")

	var calls int
	transient := errors.New("dial tcp: connection refused")
	for i := 0; i < 2; i++ {
		done, err := o.deliverOnce(context.Background(), id, func(_ context.Context, _ *OutboxItem) (bool, error) {
			calls++
			return false, transient
		})
		if done {
			t.Fatalf("attempt %d should not be done", i)
		}
		if err == nil {
			t.Fatalf("attempt %d should return error", i)
		}
		time.Sleep(3 * time.Millisecond) // let backoff window pass
	}
	done, _ := o.deliverOnce(context.Background(), id, func(_ context.Context, _ *OutboxItem) (bool, error) {
		calls++
		return true, nil
	})
	if !done {
		t.Fatal("third attempt should complete")
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	if o.PendingCount() != 0 {
		t.Fatalf("PendingCount = %d, want 0", o.PendingCount())
	}
}

func TestOutbox_ChunkProgressPersisted(t *testing.T) {
	dir := t.TempDir()
	cfg := testOutboxCfg()
	o := NewOutbox(dir, cfg)
	id := o.Add("feishu", "k", []byte(`{}`), "b", "")
	o.SetChunkProgress(id, 2)

	// Simulate restart: a fresh outbox over the same directory.
	o2 := NewOutbox(dir, cfg)
	if err := o2.Load(); err != nil {
		t.Fatal(err)
	}
	o2.mu.Lock()
	it := o2.items[id]
	o2.mu.Unlock()
	if it == nil {
		t.Fatal("expected item recovered after restart")
	}
	if it.SentChunks != 2 {
		t.Fatalf("SentChunks = %d, want 2", it.SentChunks)
	}
	if it.Platform != "feishu" || it.SessionKey != "k" || it.Body != "b" {
		t.Fatalf("recovered item mismatch: %+v", it)
	}
}

func TestOutbox_MaxAttemptsMarksDead(t *testing.T) {
	dir := t.TempDir()
	cfg := testOutboxCfg()
	cfg.MaxAttempts = 2
	o := NewOutbox(dir, cfg)
	id := o.Add("feishu", "k", []byte(`{}`), "b", "")
	transient := errors.New("i/o timeout")
	for i := 0; i < 2; i++ {
		time.Sleep(2 * time.Millisecond)
		o.deliverOnce(context.Background(), id, func(_ context.Context, _ *OutboxItem) (bool, error) {
			return false, transient
		})
	}
	if o.PendingCount() != 0 {
		t.Fatalf("item should be dead after MaxAttempts, pending=%d", o.PendingCount())
	}
	if _, err := os.Stat(filepath.Join(dir, id+".json")); !os.IsNotExist(err) {
		t.Fatal("dead item file should be removed")
	}
}

func TestOutbox_ExpiredByAgeDroppedOnLoad(t *testing.T) {
	dir := t.TempDir()
	cfg := testOutboxCfg()
	o := NewOutbox(dir, cfg)
	id := o.Add("feishu", "k", []byte(`{}`), "b", "")
	// Age it beyond MaxAge.
	o.mu.Lock()
	o.items[id].CreatedAt = time.Now().Add(-31 * time.Minute)
	o.persistLocked(o.items[id])
	o.mu.Unlock()

	o2 := NewOutbox(dir, cfg)
	if err := o2.Load(); err != nil {
		t.Fatal(err)
	}
	if o2.PendingCount() != 0 {
		t.Fatalf("expired item should be dropped on load, pending=%d", o2.PendingCount())
	}
}

func TestOutbox_DueOrderedOldestFirst(t *testing.T) {
	dir := t.TempDir()
	o := NewOutbox(dir, testOutboxCfg())
	now := time.Now()
	o.mu.Lock()
	for i, key := range []string{"oldest", "middle", "newest"} {
		it := &OutboxItem{ID: key, SessionKey: "s", CreatedAt: now.Add(time.Duration(i) * time.Second), NextAt: now}
		o.items[key] = it
	}
	due := o.dueLocked(now.Add(time.Minute))
	o.mu.Unlock()
	if len(due) != 3 {
		t.Fatalf("due len = %d, want 3", len(due))
	}
	if due[0].ID != "oldest" || due[1].ID != "middle" || due[2].ID != "newest" {
		t.Fatalf("wrong order: %s,%s,%s", due[0].ID, due[1].ID, due[2].ID)
	}
}

func TestOutbox_DisabledIsNoop(t *testing.T) {
	cfg := testOutboxCfg()
	cfg.Enabled = false
	o := NewOutbox(t.TempDir(), cfg)
	if o.Enabled() {
		t.Fatal("should be disabled")
	}
	if id := o.Add("feishu", "k", nil, "b", ""); id != "" {
		t.Fatalf("Add on disabled outbox returned %q, want empty", id)
	}
	o.Kick() // must not block/panic
	if o.PendingCount() != 0 {
		t.Fatal("disabled outbox must stay empty")
	}
}

func TestOutbox_RunRedeliversOnKick(t *testing.T) {
	dir := t.TempDir()
	cfg := testOutboxCfg()
	cfg.SweepInterval = time.Hour
	o := NewOutbox(dir, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls int
	go o.Run(ctx, func(_ context.Context, _ *OutboxItem) (bool, error) {
		calls++
		if calls < 2 {
			return false, errors.New("connection refused")
		}
		return true, nil
	})
	id := o.Add("feishu", "k", []byte(`{}`), "b", "")
	// Immediate send failed and yielded: Fail releases the immediate hold so
	// the replayer may take over (a still-held item stays blocked from sweep).
	if ok := o.Fail(id, errors.New("immediate send failed")); !ok {
		t.Fatal("immediate Fail should keep the item for replay")
	}
	// First attempt happens on Add->Kick; wait, then kick again after backoff.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		if o.PendingCount() == 0 {
			break
		}
		o.Kick()
	}
	if o.PendingCount() != 0 {
		t.Fatalf("reply not redelivered, pending=%d calls=%d", o.PendingCount(), calls)
	}
	if calls < 2 {
		t.Fatalf("expected at least one retry, calls=%d", calls)
	}
	if _, err := os.Stat(filepath.Join(dir, id+".json")); !os.IsNotExist(err) {
		t.Fatal("redelivered item file should be removed")
	}
}

// Regression for duplicate final replies: while the immediate send path owns
// the item (held), the replayer sweep must never race it, no matter how overdue
// the item looks or how slowly the immediate send progresses.
func TestOutbox_ImmediateHoldBlocksSweep(t *testing.T) {
	o := NewOutbox(t.TempDir(), testOutboxCfg())
	id := o.Add("feishu", "k", []byte(`{}`), "b", "")

	// Force it overdue: a held item must still be invisible to the sweep.
	o.mu.Lock()
	o.items[id].NextAt = time.Now().Add(-time.Minute)
	due := o.dueLocked(time.Now())
	o.mu.Unlock()
	for _, it := range due {
		if it.ID == id {
			t.Fatal("held item must not be due while the immediate send is in flight")
		}
	}

	calls := 0
	o.setSender(func(context.Context, *OutboxItem) (bool, error) {
		calls++
		return true, nil
	})
	o.sweep(context.Background())
	if calls != 0 {
		t.Fatalf("sweep invoked sender %d times on a held item, want 0", calls)
	}

	// Immediate success removes the item -> nothing left to replay.
	o.Complete(id)
	if o.PendingCount() != 0 {
		t.Fatalf("PendingCount=%d after Complete, want 0", o.PendingCount())
	}
}

// Once the immediate path reports a retryable error (Fail), the hold is
// released and the replayer may take over after the backoff window.
func TestOutbox_ImmediateFailReleasesHoldForReplay(t *testing.T) {
	o := NewOutbox(t.TempDir(), testOutboxCfg())
	id := o.Add("feishu", "k", []byte(`{}`), "b", "")
	if ok := o.Fail(id, errors.New("connection reset by peer")); !ok {
		t.Fatal("Fail should keep the item scheduled for replay")
	}
	o.mu.Lock()
	held := o.items[id].immediateHeld
	o.items[id].NextAt = time.Now().Add(-time.Second) // backoff elapsed
	due := o.dueLocked(time.Now())
	o.mu.Unlock()
	if held {
		t.Fatal("Fail must release the immediate hold")
	}
	if len(due) != 1 || due[0].ID != id {
		t.Fatalf("released item should be due exactly once, got %d", len(due))
	}
}

// The immediate hold is in-process only: after a restart the item is loaded
// without the hold (so recovery can replay) while its idempotency UUID survives.
func TestOutbox_HoldReleasedAndUUIDPersistedAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := testOutboxCfg()
	o := NewOutbox(dir, cfg)
	id := o.Add("feishu", "k", []byte(`{}`), "b", "")
	wantUUID := o.ItemUUID(id)
	if wantUUID == "" {
		t.Fatal("expected a non-empty idempotency uuid")
	}

	o2 := NewOutbox(dir, cfg)
	if err := o2.Load(); err != nil {
		t.Fatal(err)
	}
	o2.mu.Lock()
	it := o2.items[id]
	o2.mu.Unlock()
	if it == nil {
		t.Fatal("item should be recovered after restart")
	}
	if it.immediateHeld {
		t.Fatal("immediate hold must NOT persist across restart")
	}
	if it.UUID != wantUUID {
		t.Fatalf("uuid not persisted: got %q want %q", it.UUID, wantUUID)
	}
}

func TestOutbox_UUIDUniquePerReply(t *testing.T) {
	o := NewOutbox(t.TempDir(), testOutboxCfg())
	a := o.Add("feishu", "k1", nil, "x", "")
	b := o.Add("feishu", "k2", nil, "y", "")
	ua, ub := o.ItemUUID(a), o.ItemUUID(b)
	if ua == "" || ub == "" || ua == ub {
		t.Fatalf("uuids must be non-empty and unique: %q %q", ua, ub)
	}
}

func TestOutboxCtxUUIDRoundTrip(t *testing.T) {
	if OutboxUUIDFromContext(nil) != "" {
		t.Fatal("nil ctx must yield empty uuid")
	}
	base := context.Background()
	if OutboxUUIDFromContext(base) != "" {
		t.Fatal("ctx without uuid must yield empty")
	}
	if WithOutboxUUID(base, "") != base {
		t.Fatal("empty uuid must return ctx unchanged")
	}
	want := "uuid-xyz"
	if got := OutboxUUIDFromContext(WithOutboxUUID(base, want)); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// Stop must halt the replayer and leave the data directory quiescent: later
// mutating calls short-circuit disk I/O, and Stop is idempotent / non-blocking.
func TestOutbox_StopQuiescesDisk(t *testing.T) {
	dir := t.TempDir()
	cfg := testOutboxCfg()
	cfg.SweepInterval = time.Hour
	o := NewOutbox(dir, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.Run(ctx, func(context.Context, *OutboxItem) (bool, error) { return true, nil })

	id := o.Add("feishu", "k", []byte(`{}`), "b", "")
	if _, err := os.Stat(filepath.Join(dir, id+".json")); err != nil {
		t.Fatalf("item should be on disk before Stop: %v", err)
	}

	done := make(chan struct{})
	go func() { o.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop blocked; replayer did not unwind")
	}
	o.Stop() // idempotent: must not panic or block

	// After Stop, further mutations must not create/remove files on disk.
	o.SetChunkProgress(id, 1)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("disk mutated after Stop: %d entries, want 1", len(entries))
	}
}
