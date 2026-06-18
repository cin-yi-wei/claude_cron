package channelagent

import (
	"context"
	"sync"
)

// ChatHub is the in-process pub/sub bus for the "web" platform: it bridges a
// binding's reply stream (written by its Claude session, delivered by WebSender)
// to any browsers subscribed over SSE. Keyed by binding name. It lives in the
// serve process, which also hosts the admin HTTP server — so a package-level
// singleton (DefaultChatHub) is the shared instance both halves use.
//
// Sends are non-blocking: a slow/disconnected subscriber whose buffer is full is
// skipped rather than stalling the sender (replies are also persisted in the
// session transcript, so a missed live event is not data loss).
type ChatHub struct {
	mu   sync.Mutex
	subs map[string]map[int]chan ChatEvent
	next int
}

// ChatEvent is one message in a web chat: a session reply (role=assistant) or a
// user message echoed back so every connected tab stays in sync (role=user).
type ChatEvent struct {
	Role string `json:"role"`
	Text string `json:"text"`
	Time string `json:"time,omitempty"`
}

func NewChatHub() *ChatHub {
	return &ChatHub{subs: map[string]map[int]chan ChatEvent{}}
}

// DefaultChatHub is the shared hub used by serve (WebSender) and the admin SSE
// endpoints. Both run in-process, so they reference the same instance.
var DefaultChatHub = NewChatHub()

// Subscribe registers a listener for key and returns its event channel plus an
// unsubscribe func that must be called when the subscriber goes away.
func (h *ChatHub) Subscribe(key string) (<-chan ChatEvent, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[key] == nil {
		h.subs[key] = map[int]chan ChatEvent{}
	}
	id := h.next
	h.next++
	ch := make(chan ChatEvent, 32)
	h.subs[key][id] = ch
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if m := h.subs[key]; m != nil {
			if c, ok := m[id]; ok {
				close(c)
				delete(m, id)
			}
			if len(m) == 0 {
				delete(h.subs, key)
			}
		}
	}
}

// Publish fans an event out to all subscribers of key (non-blocking).
func (h *ChatHub) Publish(key string, ev ChatEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs[key] {
		select {
		case ch <- ev:
		default: // subscriber buffer full → drop (don't block the sender)
		}
	}
}

// Subscribers reports how many live listeners key has (used by tests/diagnostics).
func (h *ChatHub) Subscribers(key string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[key])
}

// WebSender is the Sender for web-platform bindings: instead of calling an
// external chat API, it publishes the session's reply text to the ChatHub, which
// streams it to subscribed browsers. Key is the binding name.
type WebSender struct {
	Hub *ChatHub
	Key string
}

func (s WebSender) Send(ctx context.Context, out OutputJob) error {
	if s.Hub != nil {
		s.Hub.Publish(s.Key, ChatEvent{Role: "assistant", Text: out.Text})
	}
	return nil
}
