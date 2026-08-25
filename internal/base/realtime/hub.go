// Package realtime provides process-local event delivery for connected clients.
package realtime

import "sync"

const subscriberBufferSize = 16

// Event is delivered to every active connection for one workspace user.
type Event struct {
	Type           string `json:"type"`
	ConversationID string `json:"conversation_id,omitempty"`
	Payload        any    `json:"payload,omitempty"`
}

// Hub broadcasts events within one process. Cross-process fan-out for multiple
// server replicas (for example through Redis or NATS) is intentionally future work.
type Hub struct {
	mu     sync.Mutex
	subs   map[string]map[int]chan Event
	nextID int
}

// NewHub creates an empty process-local event hub.
func NewHub() *Hub {
	return &Hub{subs: make(map[string]map[int]chan Event)}
}

// Subscribe registers a buffered event channel for one workspace user.
func (h *Hub) Subscribe(workspaceID, userID string) (<-chan Event, func()) {
	h.mu.Lock()
	if h.subs == nil {
		h.subs = make(map[string]map[int]chan Event)
	}
	key := subscriptionKey(workspaceID, userID)
	if h.subs[key] == nil {
		h.subs[key] = make(map[int]chan Event)
	}
	id := h.nextID
	h.nextID++
	events := make(chan Event, subscriberBufferSize)
	h.subs[key][id] = events
	h.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()

			subscribers := h.subs[key]
			if _, ok := subscribers[id]; !ok {
				return
			}
			delete(subscribers, id)
			close(events)
			if len(subscribers) == 0 {
				delete(h.subs, key)
			}
		})
	}

	return events, unsubscribe
}

// Publish delivers an event without waiting for slow subscribers. Events are
// dropped for any subscriber whose buffer is full.
func (h *Hub) Publish(workspaceID, userID string, event Event) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, events := range h.subs[subscriptionKey(workspaceID, userID)] {
		select {
		case events <- event:
		default:
		}
	}
}

// SubscriberCount reports the number of active connections for one workspace user.
func (h *Hub) SubscriberCount(workspaceID, userID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[subscriptionKey(workspaceID, userID)])
}

func subscriptionKey(workspaceID, userID string) string {
	return workspaceID + "\x00" + userID
}
