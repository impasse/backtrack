package api

import (
	"context"
	"sync"
	"time"

	"backtrack/internal/media"
)

const livePartRetention = 12 * time.Second
const maxLivePartBytes = 32 << 20

type livePartHub struct {
	mu      sync.Mutex
	parts   map[string]map[string]media.Part
	latest  map[string]media.Part
	changed map[string]chan struct{}
}

func newLivePartHub() *livePartHub {
	return &livePartHub{
		parts:   make(map[string]map[string]media.Part),
		latest:  make(map[string]media.Part),
		changed: make(map[string]chan struct{}),
	}
}

func (h *livePartHub) publish(part media.Part) {
	h.mu.Lock()
	defer h.mu.Unlock()
	byID := h.parts[part.StreamID]
	if byID == nil {
		byID = make(map[string]media.Part)
		h.parts[part.StreamID] = byID
	}
	byID[part.ID] = part
	latest := h.latest[part.StreamID]
	if latest.ID == "" || part.EndTime.After(latest.EndTime) {
		h.latest[part.StreamID] = part
	}
	cutoff := time.Now().Add(-livePartRetention)
	var totalBytes int64
	for _, old := range byID {
		totalBytes += int64(len(old.Data))
	}
	for totalBytes > maxLivePartBytes || hasExpiredPart(byID, cutoff) {
		oldestID := ""
		var oldest media.Part
		for id, old := range byID {
			if oldestID == "" || old.EndTime.Before(oldest.EndTime) {
				oldestID, oldest = id, old
			}
		}
		if oldestID == "" {
			break
		}
		delete(byID, oldestID)
		totalBytes -= int64(len(oldest.Data))
	}
	if channel := h.changed[part.StreamID]; channel != nil {
		close(channel)
	}
	h.changed[part.StreamID] = make(chan struct{})
}

func hasExpiredPart(parts map[string]media.Part, cutoff time.Time) bool {
	for _, part := range parts {
		if part.EndTime.Before(cutoff) {
			return true
		}
	}
	return false
}

func (h *livePartHub) list(streamID string) []media.Part {
	h.mu.Lock()
	defer h.mu.Unlock()
	byID := h.parts[streamID]
	cutoff := time.Now().Add(-livePartRetention)
	parts := make([]media.Part, 0, len(byID))
	for id, part := range byID {
		if part.EndTime.Before(cutoff) {
			delete(byID, id)
			continue
		}
		parts = append(parts, part)
	}
	return parts
}

func (h *livePartHub) waitForPart(ctx context.Context, streamID, id string) (media.Part, bool) {
	for {
		h.mu.Lock()
		if part, ok := h.parts[streamID][id]; ok && !part.EndTime.Before(time.Now().Add(-livePartRetention)) {
			h.mu.Unlock()
			return part, true
		}
		changed := h.changed[streamID]
		if changed == nil {
			changed = make(chan struct{})
			h.changed[streamID] = changed
		}
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return media.Part{}, false
		case <-changed:
		}
	}
}

func (h *livePartHub) waitForUpdate(ctx context.Context, streamID string, sequence uint64, index int) {
	for {
		h.mu.Lock()
		latest := h.latest[streamID]
		if latest.ID != "" && !latest.EndTime.Before(time.Now().Add(-livePartRetention)) && (latest.Sequence > sequence || (latest.Sequence == sequence && latest.Index >= index)) {
			h.mu.Unlock()
			return
		}
		changed := h.changed[streamID]
		if changed == nil {
			changed = make(chan struct{})
			h.changed[streamID] = changed
		}
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-changed:
		}
	}
}
