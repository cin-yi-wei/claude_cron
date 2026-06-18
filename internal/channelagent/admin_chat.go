package channelagent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Web-chat endpoints (platform "web"):
//   GET  /api/chat/<name>/stream  → SSE of ChatEvents for that binding
//   POST /api/chat/<name>/send    → inject a browser message into the inbox
//   GET  /api/chat/<name>/history → past conversation (replayed from the queues)
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

// chatBinding loads the named binding. Any platform is chattable from the
// browser: replies are teed to the ChatHub by the sender (see TeeSender), and a
// browser message is injected into the binding's inbox like any other arrival.
func (h AdminHandler) chatBinding(name string) (Binding, bool) {
	if !validBindingName(name) {
		return Binding{}, false
	}
	reg, err := LoadRegistry(h.Root)
	if err != nil {
		return Binding{}, false
	}
	return reg.Get(name)
}

func (h AdminHandler) streamChat(w http.ResponseWriter, r *http.Request, name string) {
	if _, ok := h.chatBinding(name); !ok {
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
	b, ok := h.chatBinding(name)
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

// historyChat replays the binding's past conversation so a freshly-opened chat
// window shows the existing thread instead of starting blank. Sources: processed
// user messages (inbox/done, plus in-flight pending/processing) and sent replies
// (outbox/sent, plus outbox/pending). Ordered by file mtime — a user message is
// moved to done right before its reply is written, so the order comes out right.
func (h AdminHandler) historyChat(w http.ResponseWriter, name string) {
	b, ok := h.chatBinding(name)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSONResponse(w, readChatHistory(b.Root))
}

type stampedEvent struct {
	ev ChatEvent
	ts int64 // unix nanos for stable ordering
}

func readChatHistory(bRoot string) []ChatEvent {
	var items []stampedEvent
	// User messages from the inbox (done = processed; pending/processing = in flight).
	for _, sub := range []string{"done", "processing", "pending"} {
		dir := pathIn(bRoot, "inbox", sub)
		for _, fp := range jsonFilesByMtime(dir) {
			var job InputJob
			if err := ReadJSON(fp.path, &job); err != nil {
				continue
			}
			if strings.TrimSpace(job.Source.Content) == "" {
				continue
			}
			items = append(items, stampedEvent{
				ev: ChatEvent{Role: "user", Text: job.Source.Content, Time: job.Source.CreatedAt},
				ts: fp.mtime,
			})
		}
	}
	// Assistant replies from the outbox (sent; pending = not yet delivered).
	for _, sub := range []string{"sent", "pending"} {
		dir := pathIn(bRoot, "outbox", sub)
		for _, fp := range jsonFilesByMtime(dir) {
			var out OutputJob
			if err := ReadJSON(fp.path, &out); err != nil {
				continue
			}
			if !out.Send || strings.TrimSpace(out.Text) == "" {
				continue
			}
			items = append(items, stampedEvent{
				ev: ChatEvent{Role: "assistant", Text: out.Text},
				ts: fp.mtime,
			})
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].ts < items[j].ts })
	out := make([]ChatEvent, 0, len(items))
	for _, it := range items {
		out = append(out, it.ev)
	}
	return out
}

type fileMtime struct {
	path  string
	mtime int64
}

// jsonFilesByMtime lists *.json files in dir with their modtime (nanos). Missing
// dir → empty.
func jsonFilesByMtime(dir string) []fileMtime {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []fileMtime
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, fileMtime{path: filepath.Join(dir, e.Name()), mtime: info.ModTime().UnixNano()})
	}
	return out
}
