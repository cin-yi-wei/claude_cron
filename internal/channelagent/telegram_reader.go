package channelagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// TelegramReader is the single getUpdates poller for one bot token. It fetches
// updates with a persisted, advancing offset and routes each message to a
// destination keyed by chat id. Having exactly one reader per token (instead of
// one TelegramSource.Fetch per binding/control plane) avoids the 409 Conflict
// from concurrent getUpdates calls and the 24h backlog re-fetch that came from
// never advancing the offset.
type TelegramReader struct {
	BaseURL    string
	Token      string
	OffsetPath string // persists the next update_id to fetch from
	Client     *http.Client
}

// tgOffset is the persisted getUpdates cursor.
type tgOffset struct {
	Offset int64 `json:"offset"`
}

// Drain fetches pending updates once and delivers each to the route registered
// for its chat id (unrouted chats are dropped), then advances + persists the
// offset past everything fetched so those updates are never seen again. A route
// returning an error leaves the offset un-advanced for that batch so the message
// is retried next cycle.
func (r TelegramReader) Drain(ctx context.Context, routes map[string]func(context.Context, SourceMessage) error) error {
	if r.Token == "" {
		return fmt.Errorf("telegram token is required")
	}
	baseURL := r.BaseURL
	if baseURL == "" {
		baseURL = defaultTelegramBaseURL
	}
	client := r.Client
	if client == nil {
		client = http.DefaultClient
	}

	var off tgOffset
	_ = ReadJSON(r.OffsetPath, &off) // missing offset → start at 0

	endpoint, err := url.Parse(baseURL + "/bot" + r.Token + "/getUpdates")
	if err != nil {
		return err
	}
	q := endpoint.Query()
	q.Set("timeout", "0")
	if off.Offset > 0 {
		q.Set("offset", strconv.FormatInt(off.Offset, 10))
	}
	endpoint.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkHTTPResponse(resp); err != nil {
		return err
	}
	var payload telegramUpdatesResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return err
	}
	if !payload.OK {
		return fmt.Errorf("telegram getUpdates returned ok=false")
	}

	var maxID int64
	var deliverErr error
	for _, u := range payload.Result {
		if u.UpdateID > maxID {
			maxID = u.UpdateID
		}
		msg, ok := telegramExtract(u)
		if !ok {
			continue
		}
		route, ok := routes[msg.ChannelID]
		if !ok {
			continue // not a chat we serve
		}
		if err := route(ctx, msg); err != nil && deliverErr == nil {
			deliverErr = err
		}
	}
	// Only advance the offset when every delivery succeeded, so a failed write is
	// retried (Telegram re-serves un-confirmed updates).
	if deliverErr != nil {
		return deliverErr
	}
	if maxID >= off.Offset {
		off.Offset = maxID + 1
		if err := AtomicWriteJSON(r.OffsetPath, off); err != nil {
			return err
		}
	}
	return nil
}

// inboundRoutes builds the channel/chat-id → delivery map for ONE platform's
// single-connection ingest: every binding of that platform delivers to its own
// inbox, every control plane of that platform delivers to its buffer. This is the
// shared router both transports feed — the Telegram reader/webhook today, and
// (Phase B) a single Discord Gateway demux — so the routing logic is identical
// across platforms; only the connect+decode layer differs per platform.
func inboundRoutes(root string, cfg Config, reg Registry, platform string) map[string]func(context.Context, SourceMessage) error {
	routes := map[string]func(context.Context, SourceMessage) error{}
	for _, b := range reg.Bindings {
		if b.PlatformOf() != platform {
			continue
		}
		broot := b.Root
		routes[b.ChannelID] = func(ctx context.Context, msg SourceMessage) error {
			_, err := IngestMessages(ctx, broot, []SourceMessage{msg})
			return err
		}
	}
	for _, plane := range cfg.ControlPlanes() {
		if plane.Platform != platform || plane.ChannelID == "" {
			continue
		}
		buf := controlBufferPath(root, plane.Name)
		routes[plane.ChannelID] = func(_ context.Context, msg SourceMessage) error {
			return appendTelegramBuffer(buf, msg)
		}
	}
	return routes
}

// controlBufferPath is where a control plane's inbound messages are buffered by
// the shared router for the plane's BufferSource to drain. The telegram plane
// keeps its existing filename (tg_buffer.json) for back-compat.
func controlBufferPath(root, planeName string) string {
	name := "inbound_buffer.json"
	if planeName == PlatformTelegram {
		name = "tg_buffer.json"
	}
	return pathIn(ControlBindingFor(root, planeName).Root, "state", name)
}

// telegramRoutes is the Telegram-specific view of inboundRoutes, used by the poll
// reader and the webhook demux handler.
func telegramRoutes(root string, cfg Config, reg Registry) map[string]func(context.Context, SourceMessage) error {
	return inboundRoutes(root, cfg, reg, PlatformTelegram)
}

// TelegramDemuxHandler is the webhook counterpart to TelegramReader: Telegram
// POSTs every update for the bot to one endpoint, and this handler routes each
// by chat id (reloading the registry per request so new bindings are picked up).
// Used when the whole bot is in webhook mode (webhook ⊥ getUpdates per bot).
type TelegramDemuxHandler struct {
	Root   string
	Cfg    Config
	Secret string
}

func (h TelegramDemuxHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.Secret != "" && r.Header.Get("X-Telegram-Bot-Api-Secret-Token") != h.Secret {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var update telegramUpdate
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if msg, ok := telegramExtract(update); ok {
		reg, _ := LoadRegistry(h.Root)
		if route, ok := telegramRoutes(h.Root, h.Cfg, reg)[msg.ChannelID]; ok {
			if err := route(r.Context(), msg); err != nil {
				http.Error(w, "deliver failed", http.StatusInternalServerError)
				return
			}
		}
	}
	// Always 200 for an authorized, well-formed post (even if unrouted) so
	// Telegram does not retry indefinitely.
	w.WriteHeader(http.StatusOK)
}

// TelegramBufferSource is a MessageSource backed by a file the TelegramReader
// appends routed control-plane messages to. Fetch returns and clears the buffer,
// so a control plane consumes its messages without doing its own getUpdates.
type TelegramBufferSource struct {
	Path string
}

func (s TelegramBufferSource) Fetch(ctx context.Context) ([]SourceMessage, error) {
	var msgs []SourceMessage
	if err := ReadJSON(s.Path, &msgs); err != nil {
		return nil, nil // missing/empty buffer → no messages
	}
	if len(msgs) == 0 {
		return nil, nil
	}
	if err := AtomicWriteJSON(s.Path, []SourceMessage{}); err != nil {
		return nil, err
	}
	return msgs, nil
}

// appendTelegramBuffer adds a message to a control plane's buffer file (the
// reader's delivery target for control chats). Read-modify-write is safe because
// the reader and the control Fetch run sequentially in one supervisor cycle.
func appendTelegramBuffer(path string, msg SourceMessage) error {
	var msgs []SourceMessage
	_ = ReadJSON(path, &msgs)
	msgs = append(msgs, msg)
	return AtomicWriteJSON(path, msgs)
}
