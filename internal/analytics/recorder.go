// Package analytics drains click events in the background so redirects never
// wait on a database write.
package analytics

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kesha/urlshortener/internal/click"
)

// Writer persists one event.
type Writer interface {
	RecordClick(ctx context.Context, ev click.Event) error
}

// Recorder buffers events and hands them to a small worker pool.
//
// The buffer is intentionally bounded: if the database cannot keep up, dropping
// click telemetry is better than making users wait on a redirect.
type Recorder struct {
	ch        chan click.Event
	writer    Writer
	log       *slog.Logger
	wg        sync.WaitGroup
	closeOnce sync.Once
	dropped   atomic.Uint64
}

// New returns a recorder with the given buffer size.
func New(buffer int, w Writer, log *slog.Logger) *Recorder {
	if buffer <= 0 {
		buffer = 1
	}
	return &Recorder{
		ch:     make(chan click.Event, buffer),
		writer: w,
		log:    log,
	}
}

// Start launches the workers.
func (r *Recorder) Start() {
	for i := 0; i < 2; i++ {
		r.wg.Add(1)
		go r.worker()
	}
}

// Record enqueues an event without blocking. It reports via Dropped when the
// buffer is full.
func (r *Recorder) Record(ev click.Event) {
	select {
	case r.ch <- ev:
	default:
		r.dropped.Add(1)
	}
}

// Dropped is the number of events discarded because the buffer was full.
func (r *Recorder) Dropped() uint64 {
	return r.dropped.Load()
}

// Close stops accepting events and waits for the queue to drain, or for ctx to
// expire.
func (r *Recorder) Close(ctx context.Context) {
	r.closeOnce.Do(func() { close(r.ch) })

	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		r.log.Warn("click queue did not drain before shutdown", "dropped", r.Dropped())
	}
}

func (r *Recorder) worker() {
	defer r.wg.Done()
	for ev := range r.ch {
		// Each write gets its own timeout: a cancelled request context must not
		// abort telemetry for a redirect that already succeeded.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := r.writer.RecordClick(ctx, ev); err != nil {
			r.log.Warn("click write failed", "code", ev.Code, "err", err)
		}
		cancel()
	}
}
