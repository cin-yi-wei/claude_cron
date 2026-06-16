package channelagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const defaultTelegramBaseURL = "https://api.telegram.org"

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

// telegramUpdateToMessage maps a Telegram update to a SourceMessage, keeping
// only messages for chatID. Returns ok=false for non-message updates or other
// chats. Shared by getUpdates (poll) and the webhook handler (push).
func telegramUpdateToMessage(update telegramUpdate, chatID string) (SourceMessage, bool) {
	if update.Message == nil {
		return SourceMessage{}, false
	}
	message := update.Message
	if strconv.FormatInt(message.Chat.ID, 10) != chatID {
		return SourceMessage{}, false
	}
	content := message.Text
	if content == "" {
		content = message.Caption
	}
	return SourceMessage{
		Platform:  "telegram",
		ChannelID: chatID,
		MessageID: strconv.FormatInt(update.UpdateID, 10),
		AuthorID:  strconv.FormatInt(message.From.ID, 10),
		CreatedAt: time.Unix(message.Date, 0).UTC().Format(time.RFC3339),
		Content:   content,
	}, true
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
	body, err := json.Marshal(map[string]string{"chat_id": s.ChatID, "text": output.Text})
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
