package channelagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const defaultTelegramBaseURL = "https://api.telegram.org"

var tgFenceRE = regexp.MustCompile("(?s)```(\\w*)\\n?(.*?)```")

// telegramHTML converts ```lang fenced blocks to Telegram HTML
// <pre><code class="language-lang">…</code></pre> (Telegram highlights the
// language, incl. diff red/green) and HTML-escapes everything else. Used with
// parse_mode=HTML.
func telegramHTML(text string) string {
	esc := func(s string) string {
		s = strings.ReplaceAll(s, "&", "&amp;")
		s = strings.ReplaceAll(s, "<", "&lt;")
		s = strings.ReplaceAll(s, ">", "&gt;")
		return s
	}
	var out strings.Builder
	last := 0
	for _, loc := range tgFenceRE.FindAllStringSubmatchIndex(text, -1) {
		out.WriteString(esc(text[last:loc[0]]))
		lang := text[loc[2]:loc[3]]
		code := text[loc[4]:loc[5]]
		out.WriteString("<pre>")
		if lang != "" {
			out.WriteString(`<code class="language-` + lang + `">`)
		} else {
			out.WriteString("<code>")
		}
		out.WriteString(esc(code))
		out.WriteString("</code></pre>")
		last = loc[1]
	}
	out.WriteString(esc(text[last:]))
	return out.String()
}

type TelegramSource struct {
	BaseURL string
	Token   string
	ChatID  string
	Client  *http.Client
}

func (s TelegramSource) Fetch(ctx context.Context) ([]SourceMessage, error) {
	if s.Token == "" {
		return nil, fmt.Errorf("telegram token is required")
	}
	if s.ChatID == "" {
		return nil, fmt.Errorf("telegram chat id is required")
	}
	baseURL := s.BaseURL
	if baseURL == "" {
		baseURL = defaultTelegramBaseURL
	}
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}

	endpoint, err := url.Parse(baseURL + "/bot" + s.Token + "/getUpdates")
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	query.Set("timeout", "0")
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkHTTPResponse(resp); err != nil {
		return nil, err
	}

	var payload telegramUpdatesResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	if !payload.OK {
		return nil, fmt.Errorf("telegram getUpdates returned ok=false")
	}

	var messages []SourceMessage
	for _, update := range payload.Result {
		if msg, ok := telegramUpdateToMessage(update, s.ChatID); ok {
			messages = append(messages, msg)
		}
	}
	return messages, nil
}

// telegramExtract maps a Telegram update to a SourceMessage tagged with its own
// chat id (no filtering). Returns ok=false for non-message updates. Used by the
// shared reader, which routes by chat id rather than pre-filtering per consumer.
func telegramExtract(update telegramUpdate) (SourceMessage, bool) {
	if update.Message == nil {
		return SourceMessage{}, false
	}
	message := update.Message
	content := message.Text
	if content == "" {
		content = message.Caption
	}
	return SourceMessage{
		Platform:  "telegram",
		ChannelID: strconv.FormatInt(message.Chat.ID, 10),
		MessageID: strconv.FormatInt(update.UpdateID, 10),
		AuthorID:  strconv.FormatInt(message.From.ID, 10),
		CreatedAt: time.Unix(message.Date, 0).UTC().Format(time.RFC3339),
		Content:   content,
	}, true
}

// telegramUpdateToMessage maps a Telegram update to a SourceMessage, keeping
// only messages for chatID. Returns ok=false for non-message updates or other
// chats. Shared by getUpdates (poll) and the webhook handler (push).
func telegramUpdateToMessage(update telegramUpdate, chatID string) (SourceMessage, bool) {
	msg, ok := telegramExtract(update)
	if !ok || msg.ChannelID != chatID {
		return SourceMessage{}, false
	}
	return msg, true
}

type TelegramSender struct {
	BaseURL string
	Token   string
	ChatID  string
	Client  *http.Client
}

func (s TelegramSender) Send(ctx context.Context, output OutputJob) error {
	if s.Token == "" {
		return fmt.Errorf("telegram token is required")
	}
	if s.ChatID == "" {
		return fmt.Errorf("telegram chat id is required")
	}
	baseURL := s.BaseURL
	if baseURL == "" {
		baseURL = defaultTelegramBaseURL
	}
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	// Messages with ```fenced``` blocks (e.g. activity diffs) are sent as HTML so
	// Telegram renders code blocks + diff colouring; plain messages stay plain
	// (zero change for normal replies).
	payloadMap := map[string]string{"chat_id": s.ChatID, "text": output.Text}
	if strings.Contains(output.Text, "```") {
		payloadMap["text"] = telegramHTML(output.Text)
		payloadMap["parse_mode"] = "HTML"
	}
	body, err := json.Marshal(payloadMap)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/bot"+s.Token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkHTTPResponse(resp); err != nil {
		return err
	}
	var payload telegramSendResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return err
	}
	if !payload.OK {
		return fmt.Errorf("telegram sendMessage returned ok=false")
	}
	return nil
}

// SetWebhook registers webhookURL with Telegram so it POSTs updates there. If
// secret is non-empty it is set as the secret token Telegram echoes in the
// X-Telegram-Bot-Api-Secret-Token header. Used by push mode at startup.
func SetWebhook(ctx context.Context, baseURL, token, webhookURL, secret string, client *http.Client) error {
	if token == "" {
		return fmt.Errorf("telegram token is required")
	}
	if webhookURL == "" {
		return fmt.Errorf("webhook url is required")
	}
	if baseURL == "" {
		baseURL = defaultTelegramBaseURL
	}
	if client == nil {
		client = http.DefaultClient
	}
	payload := map[string]string{"url": webhookURL}
	if secret != "" {
		payload["secret_token"] = secret
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/bot"+token+"/setWebhook", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkHTTPResponse(resp); err != nil {
		return err
	}
	var out telegramSendResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	if !out.OK {
		return fmt.Errorf("telegram setWebhook returned ok=false")
	}
	return nil
}

type telegramUpdatesResponse struct {
	OK     bool             `json:"ok"`
	Result []telegramUpdate `json:"result"`
}

type telegramUpdate struct {
	UpdateID int64            `json:"update_id"`
	Message  *telegramMessage `json:"message"`
}

type telegramMessage struct {
	MessageID int64        `json:"message_id"`
	Date      int64        `json:"date"`
	Text      string       `json:"text"`
	Caption   string       `json:"caption"`
	Chat      telegramChat `json:"chat"`
	From      telegramUser `json:"from"`
}

type telegramChat struct {
	ID int64 `json:"id"`
}

type telegramUser struct {
	ID int64 `json:"id"`
}

type telegramSendResponse struct {
	OK bool `json:"ok"`
}
