package channelagent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Web-chat endpoints (platform "web"):
//   GET  /api/chat/<name>/stream  → SSE of ChatEvents for that binding
//   POST /api/chat/<name>/send    → inject a browser message into the inbox
//
// These bridge the browser to a cc-<name> Claude session via the ChatHub
// (replies) and the inbox (messages), reusing the existing worker/sender
// pipeline. EventSource can't set an Authorization header, so chat auth also
// accepts a ?token= query param (header still works for the POST).

func (h AdminHandler) chatAuthorized(r *http.Request) bool {
	if h.Token == "" {
		return true // loopback dev mode
	}
	if h.authorized(r) {
		return true
	}
	return subtleEqual(r.URL.Query().Get("token"), h.Token)
}

// webBinding loads the named binding and verifies it is a web-platform binding.
func (h AdminHandler) webBinding(name string) (Binding, bool) {
	if !validBindingName(name) {
		return Binding{}, false
	}
	reg, err := LoadRegistry(h.Root)
	if err != nil {
		return Binding{}, false
	}
	b, ok := reg.Get(name)
	if !ok || b.PlatformOf() != PlatformWeb {
		return Binding{}, false
	}
	return b, true
}

func (h AdminHandler) streamChat(w http.ResponseWriter, r *http.Request, name string) {
	if _, ok := h.webBinding(name); !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering (cloudflared)

	events, unsubscribe := DefaultChatHub.Subscribe(name)
	defer unsubscribe()

	// Initial comment flushes headers so the browser fires onopen immediately.
	fmt.Fprintf(w, ": connected to %s\n\n", name)
	flusher.Flush()

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case ev, ok := <-events:
			if !ok {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

type chatSendRequest struct {
	Text string `json:"text"`
}

func (h AdminHandler) sendChat(w http.ResponseWriter, r *http.Request, name string) {
	b, ok := h.webBinding(name)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var req chatSendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Text == "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "text required"})
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	msg := SourceMessage{
		Platform:  PlatformWeb,
		ChannelID: b.ChannelID,
		MessageID: newWebMessageID(),
		AuthorID:  "web",
		CreatedAt: now,
		Content:   req.Text,
	}
	if _, err := IngestMessages(r.Context(), b.Root, []SourceMessage{msg}); err != nil {
		http.Error(w, "ingest error", http.StatusInternalServerError)
		return
	}
	// Echo the user message to every connected tab so the conversation stays in
	// sync without each client tracking its own optimistic state.
	DefaultChatHub.Publish(name, ChatEvent{Role: "user", Text: req.Text, Time: now})
	writeJSONResponse(w, map[string]string{"result": "queued"})
}

// newWebMessageID returns a collision-resistant id for a browser message
// (nanosecond stamp + random suffix), used as the dedup key in the inbox.
func newWebMessageID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("web-%d-%s", time.Now().UnixNano(), hex.EncodeToString(b[:]))
}
