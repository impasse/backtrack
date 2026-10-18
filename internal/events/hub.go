package events

import (
	"context"
	"sync"
	"time"
)

type Event struct {
	Type      string    `json:"type"`
	StreamID  string    `json:"streamId,omitempty"`
	ClipID    string    `json:"clipId,omitempty"`
	Progress  float64   `json:"progress,omitempty"`
	FreeBytes int64     `json:"freeBytes,omitempty"`
	At        time.Time `json:"at"`
}

type Hub struct {
	mu          sync.Mutex
	subscribers map[chan Event]struct{}
}

func New() *Hub { return &Hub{subscribers: make(map[chan Event]struct{})} }

func (h *Hub) Publish(event Event) {
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for subscriber := range h.subscribers {
		select {
		case subscriber <- event:
		default:
			// Slow clients should refresh their current view instead of holding up
			// ingest, clip workers, or the retention loop.
		}
	}
}

func (h *Hub) Subscribe(ctx context.Context) <-chan Event {
	ch := make(chan Event, 32)
	h.mu.Lock()
	h.subscribers[ch] = struct{}{}
	h.mu.Unlock()
	go func() {
		<-ctx.Done()
		h.mu.Lock()
		delete(h.subscribers, ch)
		close(ch)
		h.mu.Unlock()
	}()
	return ch
}
