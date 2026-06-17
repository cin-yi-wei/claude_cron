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
