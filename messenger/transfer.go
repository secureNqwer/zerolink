// transfer.go – asynchronous media transfer manager with progress reporting.
// Each upload or download runs in its own goroutine and publishes
// TransferProgress events on the EventBus so the UI can show a progress bar.
package messenger

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/secureNqwer/zerolink/core"
)

// ─── transferManager ─────────────────────────────────────────────────────────

type transferManager struct {
	mu      sync.RWMutex
	active  map[string]*transfer
	engine  *Engine
}

func newTransferManager(e *Engine) *transferManager {
	return &transferManager{
		active: make(map[string]*transfer),
		engine: e,
	}
}

// transfer represents one in-flight file transfer.
type transfer struct {
	id        string
	msgID     core.MessageID
	chatID    core.ChatID
	direction core.TransferDirection
	state     core.TransferState
	total     int64
	done      int64    // bytes transferred so far (atomic)
	startedAt time.Time

	pauseCh  chan struct{}
	resumeCh chan struct{}
	cancelCh chan struct{}

	mu  sync.Mutex
	err string
}

// ─── Send ─────────────────────────────────────────────────────────────────────

// Send enqueues an upload of data. Returns a transfer ID immediately;
// the actual upload runs in the background.
func (tm *transferManager) Send(
	ctx context.Context,
	chatID core.ChatID,
	msgID core.MessageID,
	data []byte,
	mimeType string,
) (string, error) {
	tid := uuid.New().String()
	t := &transfer{
		id:        tid,
		msgID:     msgID,
		chatID:    chatID,
		direction: core.TransferUpload,
		state:     core.TransferPending,
		total:     int64(len(data)),
		startedAt: time.Now(),
		pauseCh:   make(chan struct{}, 1),
		resumeCh:  make(chan struct{}, 1),
		cancelCh:  make(chan struct{}, 1),
	}
	tm.mu.Lock()
	tm.active[tid] = t
	tm.mu.Unlock()

	go tm.runUpload(ctx, t, data)
	return tid, nil
}

// Receive enqueues a download of a media item identified by its hash.
func (tm *transferManager) Receive(
	ctx context.Context,
	chatID core.ChatID,
	msgID core.MessageID,
	mediaHash string,
) (string, error) {
	tid := uuid.New().String()
	t := &transfer{
		id:        tid,
		msgID:     msgID,
		chatID:    chatID,
		direction: core.TransferDownload,
		state:     core.TransferPending,
		startedAt: time.Now(),
		pauseCh:   make(chan struct{}, 1),
		resumeCh:  make(chan struct{}, 1),
		cancelCh:  make(chan struct{}, 1),
	}
	tm.mu.Lock()
	tm.active[tid] = t
	tm.mu.Unlock()

	go tm.runDownload(ctx, t, mediaHash)
	return tid, nil
}

// Cancel cancels an in-progress or paused transfer.
func (tm *transferManager) Cancel(transferID string) error {
	t := tm.get(transferID)
	if t == nil {
		return fmt.Errorf("transfer %s not found", transferID)
	}
	t.mu.Lock()
	if t.state == core.TransferCompleted || t.state == core.TransferFailed || t.state == core.TransferCancelled {
		t.mu.Unlock()
		return errors.New("transfer already finished")
	}
	t.state = core.TransferCancelled
	t.mu.Unlock()

	select {
	case t.cancelCh <- struct{}{}:
	default:
	}
	return nil
}

// Pause temporarily suspends an active transfer.
func (tm *transferManager) Pause(transferID string) error {
	t := tm.get(transferID)
	if t == nil {
		return fmt.Errorf("transfer %s not found", transferID)
	}
	t.mu.Lock()
	if t.state != core.TransferActive {
		t.mu.Unlock()
		return errors.New("transfer is not active")
	}
	t.state = core.TransferPaused
	t.mu.Unlock()

	select {
	case t.pauseCh <- struct{}{}:
	default:
	}
	return nil
}

// Resume resumes a paused transfer.
func (tm *transferManager) Resume(transferID string) error {
	t := tm.get(transferID)
	if t == nil {
		return fmt.Errorf("transfer %s not found", transferID)
	}
	t.mu.Lock()
	if t.state != core.TransferPaused {
		t.mu.Unlock()
		return errors.New("transfer is not paused")
	}
	t.state = core.TransferActive
	t.mu.Unlock()

	select {
	case t.resumeCh <- struct{}{}:
	default:
	}
	return nil
}

// Status returns the current progress of a transfer.
func (tm *transferManager) Status(transferID string) (*core.TransferProgress, error) {
	t := tm.get(transferID)
	if t == nil {
		return nil, fmt.Errorf("transfer %s not found", transferID)
	}
	return t.progress(), nil
}

// ─── Upload worker ────────────────────────────────────────────────────────────

const chunkSize = 64 * 1024 // 64 KiB chunks

func (tm *transferManager) runUpload(ctx context.Context, t *transfer, data []byte) {
	defer tm.remove(t.id)

	t.mu.Lock()
	t.state = core.TransferActive
	t.mu.Unlock()
	tm.publish(t)

	total := int64(len(data))
	var uploaded int64
	speedStart := time.Now()
	var speedBytes int64

	for uploaded < total {
		// Check cancel
		select {
		case <-t.cancelCh:
			tm.finish(t, core.TransferCancelled, "")
			return
		case <-ctx.Done():
			tm.finish(t, core.TransferCancelled, ctx.Err().Error())
			return
		default:
		}

		// Check pause
		t.mu.Lock()
		paused := t.state == core.TransferPaused
		t.mu.Unlock()
		if paused {
			tm.publish(t)
			select {
			case <-t.resumeCh:
			case <-t.cancelCh:
				tm.finish(t, core.TransferCancelled, "")
				return
			case <-ctx.Done():
				tm.finish(t, core.TransferCancelled, ctx.Err().Error())
				return
			}
			t.mu.Lock()
			t.state = core.TransferActive
			t.mu.Unlock()
		}

		end := uploaded + chunkSize
		if end > total {
			end = total
		}
		chunk := data[uploaded:end]

		// Store chunk to local media store (server CDN upload would go here)
		if err := tm.engine.store.SaveMedia(
			fmt.Sprintf("%s-chunk-%d", t.msgID, uploaded),
			chunk,
		); err != nil {
			tm.finish(t, core.TransferFailed, err.Error())
			tm.engine.log.Error("upload chunk failed", zap.Error(err))
			return
		}

		uploaded += int64(len(chunk))
		atomic.StoreInt64(&t.done, uploaded)
		speedBytes += int64(len(chunk))

		// Calculate speed every second
		if elapsed := time.Since(speedStart); elapsed >= time.Second {
			// speed is computed inside progress()
			speedStart = time.Now()
			speedBytes = 0
		}

		tm.publish(t)

		// Small yield to avoid spinning at 100% CPU on large files
		time.Sleep(time.Millisecond)
	}

	// Upload to server CDN if connected
	if tm.engine.serverConn != nil && tm.engine.serverConn.Connected() {
		// In production: HTTP PUT /media/<hash> to server
		// Here we just mark it done
	}

	tm.finish(t, core.TransferCompleted, "")
}

// ─── Download worker ──────────────────────────────────────────────────────────

func (tm *transferManager) runDownload(ctx context.Context, t *transfer, mediaHash string) {
	defer tm.remove(t.id)

	t.mu.Lock()
	t.state = core.TransferActive
	t.mu.Unlock()
	tm.publish(t)

	// Try local cache first
	if data, err := tm.engine.store.GetMedia(mediaHash); err == nil && len(data) > 0 {
		atomic.StoreInt64(&t.total, int64(len(data)))
		atomic.StoreInt64(&t.done, int64(len(data)))
		tm.finish(t, core.TransferCompleted, "")
		return
	}

	// Try server CDN
	if tm.engine.serverConn != nil && tm.engine.serverConn.Connected() {
		data, err := tm.engine.serverConn.FetchMedia(ctx, mediaHash)
		if err == nil && len(data) > 0 {
			atomic.StoreInt64(&t.total, int64(len(data)))
			atomic.StoreInt64(&t.done, int64(len(data)))
			tm.engine.store.SaveMedia(mediaHash, data)
			tm.finish(t, core.TransferCompleted, "")
			return
		}
	}

	// TODO: request from peer directly via custom protocol
	tm.finish(t, core.TransferFailed, "media not available")
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func (tm *transferManager) get(id string) *transfer {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	return tm.active[id]
}

func (tm *transferManager) remove(id string) {
	tm.mu.Lock()
	delete(tm.active, id)
	tm.mu.Unlock()
}

func (tm *transferManager) finish(t *transfer, state core.TransferState, errMsg string) {
	t.mu.Lock()
	t.state = state
	t.err = errMsg
	t.mu.Unlock()
	tm.publish(t)
}

func (tm *transferManager) publish(t *transfer) {
	tm.engine.bus.Publish(core.Event{
		Type:      core.EvtTransferProgress,
		Timestamp: time.Now(),
		Data:      t.progress(),
	})
}

func (t *transfer) progress() *core.TransferProgress {
	t.mu.Lock()
	state := t.state
	errMsg := t.err
	t.mu.Unlock()

	done := atomic.LoadInt64(&t.done)
	total := atomic.LoadInt64(&t.total)

	var pct float64
	if total > 0 {
		pct = float64(done) / float64(total) * 100
	}

	elapsed := time.Since(t.startedAt).Seconds()
	var speed int64
	if elapsed > 0 {
		speed = int64(float64(done) / elapsed)
	}

	return &core.TransferProgress{
		TransferID: t.id,
		MessageID:  t.msgID,
		ChatID:     t.chatID,
		Direction:  t.direction,
		State:      state,
		BytesTotal: total,
		BytesDone:  done,
		Percent:    pct,
		SpeedBps:   speed,
		Error:      errMsg,
	}
}
