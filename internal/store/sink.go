package store

import (
	"sync"
	"sync/atomic"

	"github.com/sentania-labs/benchwarmer/internal/events"
)

// maxBatch bounds how many buffered events one transaction writes.
const maxBatch = 256

// EventSink is an events.Sink that never blocks the caller. Events go into a
// bounded buffer; a background writer inserts them in batches. When the
// buffer is full the event is dropped and counted, because stalling the
// controller on disk I/O is worse than losing an audit row.
type EventSink struct {
	st   *Store
	ch   chan events.Event
	done chan struct{}

	mu     sync.RWMutex // guards closed and the close of ch
	closed bool

	dropped atomic.Uint64
	failed  atomic.Uint64
	onErr   atomic.Pointer[func(error)]
}

// NewEventSink starts a sink writing to st with room for bufferSize pending
// events (minimum 1).
func NewEventSink(st *Store, bufferSize int) *EventSink {
	s := &EventSink{st: st, ch: make(chan events.Event, max(bufferSize, 1)), done: make(chan struct{})}
	go s.run()
	return s
}

// OnError registers a callback for write failures (called from the writer
// goroutine). It must not block and must not log event contents.
func (s *EventSink) OnError(fn func(error)) { s.onErr.Store(&fn) }

// Emit queues e without blocking. After Close, events are dropped.
func (s *EventSink) Emit(e events.Event) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		s.dropped.Add(1)
		return
	}
	select {
	case s.ch <- e:
	default:
		s.dropped.Add(1)
	}
}

// Dropped is how many events were discarded because the buffer was full or
// the sink was closed.
func (s *EventSink) Dropped() uint64 { return s.dropped.Load() }

// Failed is how many events were lost to database write errors.
func (s *EventSink) Failed() uint64 { return s.failed.Load() }

// Close stops accepting events, writes everything already buffered, and
// waits for the writer to finish. It is idempotent.
func (s *EventSink) Close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
	s.mu.Unlock()
	<-s.done
}

func (s *EventSink) run() {
	defer close(s.done)
	batch := make([]events.Event, 0, maxBatch)
	for e := range s.ch {
		batch = append(batch[:0], e)
	fill:
		for len(batch) < maxBatch {
			select {
			case e, ok := <-s.ch:
				if !ok {
					break fill
				}
				batch = append(batch, e)
			default:
				break fill
			}
		}
		if _, err := s.st.AppendEvents(batch); err != nil {
			s.failed.Add(uint64(len(batch)))
			if fn := s.onErr.Load(); fn != nil {
				(*fn)(err)
			}
		}
	}
}

var _ events.Sink = (*EventSink)(nil)
