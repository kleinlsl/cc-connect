package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// outboxInitialGraceDefault is the default grace period after Add() before the
// sweep can pick up the item. This prevents the sweep from redelivering while
// the immediate send path is still in progress (race between Add→Kick and Complete).
const outboxInitialGraceDefault = 30 * time.Second

// OutboxConfig controls durable redelivery of final replies whose immediate
// delivery failed on a retryable platform/network error.
type OutboxConfig struct {
	// Enabled toggles the outbox. When false the engine keeps the legacy
	// fire-and-forget send behaviour.
	Enabled bool
	// MaxAge is the longest time an unsent reply is kept. Replies older than
	// MaxAge are dropped (marked dead) rather than delivered as confusing
	// "ghost replies" long after the conversation moved on.
	MaxAge time.Duration
	// MaxAttempts bounds how many (re)send attempts a reply gets before it is
	// dropped. 0 means unlimited (subject to MaxAge).
	MaxAttempts int
	// InitialDelay / MaxDelay parameterize exponential backoff between
	// attempts (with a small jitter).
	InitialDelay time.Duration
	MaxDelay     time.Duration
	// SweepInterval is how often the replayer wakes up to scan for due items
	// even without an explicit kick.
	SweepInterval time.Duration
	// InitialGrace is the grace period after Add() before the sweep can pick
	// up the item. Zero falls back to outboxInitialGraceDefault.
	InitialGrace time.Duration
}

// DefaultOutboxConfig returns the production defaults: enabled, 30min TTL,
// 12 attempts, backoff 5s..5min, 15s sweep, 30s initial grace.
func DefaultOutboxConfig() OutboxConfig {
	return OutboxConfig{
		Enabled:       true,
		MaxAge:        30 * time.Minute,
		MaxAttempts:   12,
		InitialDelay:  5 * time.Second,
		MaxDelay:      5 * time.Minute,
		SweepInterval: 15 * time.Second,
		InitialGrace:  outboxInitialGraceDefault,
	}
}

// OutboxItem is one final reply awaiting (re)delivery. It is persisted as a
// single JSON file under the outbox directory so an in-flight reply survives
// process restarts.
type OutboxItem struct {
	ID         string `json:"id"`
	Platform   string `json:"platform"`
	SessionKey string `json:"session_key"`
	// ReplyCtx is the platform-encoded reply context (opaque to core).
	ReplyCtx []byte `json:"reply_ctx"`
	// Body / Footer are the already-rendered final reply; the model is never
	// re-invoked on replay.
	Body       string    `json:"body"`
	Footer     string    `json:"footer"`
	SentChunks int       `json:"sent_chunks"` // number of leading chunks already acked
	Attempts   int       `json:"attempts"`
	CreatedAt  time.Time `json:"created_at"`
	// UUID is the platform idempotency key shared by immediate send and every replay.
	UUID string `json:"uuid"`
	// immediateHeld: in-process immediate send still running -> sweep must not race it.
	// Not persisted so a process restart releases the hold and recovery can replay.
	immediateHeld bool      `json:"-"`
	NextAt        time.Time `json:"next_at"`
}

func (it *OutboxItem) file(dir string) string {
	return filepath.Join(dir, it.ID+".json")
}

// OutboxSender (re)sends a single item. It returns done=true once every
// remaining chunk was delivered (the item is then removed); a non-nil err
// schedules another backoff attempt.
type OutboxSender func(ctx context.Context, it *OutboxItem) (done bool, err error)

// Outbox durably parks final replies that could not be sent immediately and
// replays them with exponential backoff. It is platform-agnostic: platforms
// opt in via ReplyContextCodec + SendErrorClassifier.
type Outbox struct {
	cfg   OutboxConfig
	dir   string
	mu    sync.Mutex
	items map[string]*OutboxItem

	senderMu sync.RWMutex
	sender   OutboxSender

	kick chan struct{}
	once sync.Once
	// runCtx/runDone let Stop halt the replayer and wait for it to finish.
	runCtx    context.Context
	runCancel context.CancelFunc
	runDone   chan struct{}
	// stopped is set under mu; once true, persist/remove skip all disk I/O.
	stopped bool
}

// NewOutbox builds an outbox rooted at dir (typically <data_dir>/outbox).
func NewOutbox(dir string, cfg OutboxConfig) *Outbox {
	if cfg.InitialDelay <= 0 {
		cfg = DefaultOutboxConfig()
	}
	runCtx, runCancel := context.WithCancel(context.Background())
	return &Outbox{
		cfg:       cfg,
		dir:       dir,
		items:     make(map[string]*OutboxItem),
		kick:      make(chan struct{}, 1),
		runCtx:    runCtx,
		runCancel: runCancel,
		runDone:   make(chan struct{}),
	}
}

// Enabled reports whether the outbox is active.
func (o *Outbox) Enabled() bool { return o != nil && o.cfg.Enabled }

func newOutboxID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "ob-" + time.Now().Format("20060102T150405.000000000")
	}
	return "ob-" + hex.EncodeToString(b[:])
}

// newOutboxUUID returns the RFC4122 idempotency key for one logical final reply;
// the immediate attempt and all replays reuse it so the platform dedupes doubles.
func newOutboxUUID() string {
	return uuid.NewString()
}

// Load recovers persisted items from disk (crash / restart recovery).
func (o *Outbox) Load() error {
	if !o.Enabled() {
		return nil
	}
	if err := os.MkdirAll(o.dir, 0o755); err != nil {
		return err
	}
	ents, err := os.ReadDir(o.dir)
	if err != nil {
		return err
	}
	now := time.Now()
	o.mu.Lock()
	defer o.mu.Unlock()
	recovered, dropped := 0, 0
	for _, ent := range ents {
		if ent.IsDir() || filepath.Ext(ent.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(o.dir, ent.Name()))
		if err != nil {
			continue
		}
		var it OutboxItem
		if err := json.Unmarshal(b, &it); err != nil {
			slog.Warn("outbox: dropping unreadable item file", "file", ent.Name(), "error", err)
			_ = os.Remove(filepath.Join(o.dir, ent.Name()))
			continue
		}
		if o.expiredLocked(&it, now) {
			_ = os.Remove(it.file(o.dir))
			dropped++
			continue
		}
		if it.NextAt.IsZero() {
			it.NextAt = now
		}
		o.items[it.ID] = &it
		recovered++
	}
	if recovered > 0 || dropped > 0 {
		slog.Info("outbox: recovered pending replies from disk", "recovered", recovered, "expired_dropped", dropped, "dir", o.dir)
	}
	return nil
}

func (o *Outbox) expiredLocked(it *OutboxItem, now time.Time) bool {
	if o.cfg.MaxAttempts > 0 && it.Attempts >= o.cfg.MaxAttempts {
		return true
	}
	if o.cfg.MaxAge > 0 && !it.CreatedAt.IsZero() && now.Sub(it.CreatedAt) > o.cfg.MaxAge {
		return true
	}
	return false
}

// persistLocked writes an item atomically. Caller holds o.mu.
func (o *Outbox) persistLocked(it *OutboxItem) {
	b, err := json.Marshal(it)
	if err != nil {
		return
	}
	if o.stopped {
		return
	}
	if err := AtomicWriteFile(it.file(o.dir), b, 0o600); err != nil {
		slog.Warn("outbox: persist item failed", "id", it.ID, "error", err)
	}
}

// removeLocked deletes an item from memory and disk. Caller holds o.mu.
func (o *Outbox) removeLocked(it *OutboxItem) {
	delete(o.items, it.ID)
	if o.stopped {
		return
	}
	if err := os.Remove(it.file(o.dir)); err != nil && !os.IsNotExist(err) {
		slog.Warn("outbox: remove item file failed", "id", it.ID, "error", err)
	}
}

// Add durably records a new pending final reply and nudges the replayer. It
// returns the item ID (empty when the outbox is disabled).
func (o *Outbox) Add(platform, sessionKey string, encodedReplyCtx []byte, body, footer string) string {
	if !o.Enabled() {
		return ""
	}
	now := time.Now()
	grace := o.cfg.InitialGrace
	if grace <= 0 {
		grace = outboxInitialGraceDefault
	}
	it := &OutboxItem{
		ID:            newOutboxID(),
		Platform:      platform,
		SessionKey:    sessionKey,
		ReplyCtx:      append([]byte(nil), encodedReplyCtx...),
		Body:          body,
		Footer:        footer,
		UUID:          newOutboxUUID(),
		immediateHeld: true, // the in-process immediate send owns this until Complete/Fail
		CreatedAt:     now,
		NextAt:        now.Add(grace), // grace period so immediate send can Complete before sweep picks up
	}
	o.mu.Lock()
	if err := os.MkdirAll(o.dir, 0o755); err == nil {
		o.items[it.ID] = it
		o.persistLocked(it)
	}
	o.mu.Unlock()
	o.Kick()
	slog.Debug("outbox: final reply parked for immediate send",
		"id", it.ID, "uuid", it.UUID, "platform", platform, "session", sessionKey)
	return it.ID
}

// ItemUUID returns the idempotency key bound to item id ("" when absent).
func (o *Outbox) ItemUUID(id string) string {
	if !o.Enabled() || id == "" {
		return ""
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if it, ok := o.items[id]; ok {
		return it.UUID
	}
	return ""
}

// SetChunkProgress records how many leading chunks were already acknowledged.
func (o *Outbox) SetChunkProgress(id string, sent int) {
	if !o.Enabled() || id == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if it, ok := o.items[id]; ok {
		it.SentChunks = sent
		o.persistLocked(it)
	}
}

// Complete removes a fully delivered item.
func (o *Outbox) Complete(id string) {
	if !o.Enabled() || id == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if it, ok := o.items[id]; ok {
		o.removeLocked(it)
	}
}

// Drop discards an item without further attempts (used for permanent errors).
func (o *Outbox) Drop(id string, reason error) {
	if !o.Enabled() || id == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if it, ok := o.items[id]; ok {
		slog.Warn("outbox: dropping reply (permanent failure)", "id", it.ID, "platform", it.Platform, "session", it.SessionKey, "error", reason)
		o.removeLocked(it)
	}
}

// Fail records a failed attempt, schedules the next backoff, and returns false
// when the item exhausted its budget and was marked dead.
func (o *Outbox) Fail(id string, sendErr error) bool {
	if !o.Enabled() || id == "" {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	it, ok := o.items[id]
	if !ok {
		return false
	}
	it.immediateHeld = false // immediate path yielded to the replayer
	it.Attempts++
	now := time.Now()
	if o.expiredLocked(it, now) {
		slog.Error("outbox: gave up redelivering reply",
			"id", it.ID, "platform", it.Platform, "session", it.SessionKey,
			"attempts", it.Attempts, "age", now.Sub(it.CreatedAt).Round(time.Second), "error", sendErr)
		o.removeLocked(it)
		return false
	}
	delay := o.cfg.InitialDelay
	for i := 0; i < it.Attempts-1; i++ {
		delay *= 2
		if delay >= o.cfg.MaxDelay {
			delay = o.cfg.MaxDelay
			break
		}
	}
	// up to +25% jitter
	jitter := time.Duration(0)
	if delay > 0 {
		var b [4]byte
		if _, err := rand.Read(b[:]); err == nil {
			jitter = time.Duration(int64(b[0])%25+1) * delay / 100
		}
	}
	it.NextAt = now.Add(delay + jitter)
	o.persistLocked(it)
	slog.Warn("outbox: final reply send failed, scheduled redelivery",
		"id", it.ID, "attempt", it.Attempts, "next_in", (delay + jitter).Round(time.Millisecond), "error", sendErr)
	return true
}

// PendingCount returns the number of replies currently parked.
func (o *Outbox) PendingCount() int {
	if !o.Enabled() {
		return 0
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.items)
}

// Kick wakes the replayer promptly (non-blocking, coalesced).
func (o *Outbox) Kick() {
	if !o.Enabled() {
		return
	}
	select {
	case o.kick <- struct{}{}:
	default:
	}
}

func (o *Outbox) setSender(s OutboxSender) {
	o.senderMu.Lock()
	o.sender = s
	o.senderMu.Unlock()
}

// dueLocked returns items due now, oldest-first (preserves per-session order).
// Caller holds o.mu.
func (o *Outbox) dueLocked(now time.Time) []*OutboxItem {
	due := make([]*OutboxItem, 0, len(o.items))
	for _, it := range o.items {
		if it.immediateHeld {
			continue // immediate send still in flight; never race it
		}
		if !it.NextAt.After(now) {
			due = append(due, it)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		return due[i].CreatedAt.Before(due[j].CreatedAt)
	})
	return due
}

// Run is the replayer loop; it blocks until ctx is cancelled.
// Stop cancels the replayer and waits for it to unwind. After it returns the
// outbox performs no further disk mutation (persist/remove short-circuit), so
// the data directory is safe to tear down. Safe to call when never Run.
func (o *Outbox) Stop() {
	if o == nil || !o.Enabled() {
		return
	}
	o.mu.Lock()
	already := o.stopped
	o.stopped = true
	if o.runCancel != nil {
		o.runCancel()
	}
	done := o.runDone
	o.mu.Unlock()
	if already || done == nil {
		return
	}
	<-done
}

func (o *Outbox) Run(ctx context.Context, sender OutboxSender) {
	o.setSender(sender)
	defer close(o.runDone)
	if !o.Enabled() {
		select {
		case <-ctx.Done():
		case <-o.runCtx.Done():
		}
		return
	}
	interval := o.cfg.SweepInterval
	if interval <= 0 {
		interval = DefaultOutboxConfig().SweepInterval
	}
	o.sweep(ctx) // initial recovery pass
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-o.runCtx.Done():
			return
		case <-ticker.C:
			o.sweep(ctx)
		case <-o.kick:
			o.sweep(ctx)
		}
	}
}

func (o *Outbox) sweep(ctx context.Context) {
	o.senderMu.RLock()
	sender := o.sender
	o.senderMu.RUnlock()
	if sender == nil || ctx.Err() != nil {
		return
	}
	now := time.Now()
	o.mu.Lock()
	due := o.dueLocked(now)
	o.mu.Unlock()
	for _, it := range due {
		if ctx.Err() != nil {
			return
		}
		done, err := sender(ctx, it)
		switch {
		case done:
			o.Complete(it.ID)
		case err != nil:
			o.Fail(it.ID, err)
		}
	}
}
