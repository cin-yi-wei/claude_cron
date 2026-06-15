package channelagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

const defaultDiscordBaseURL = "https://discord.com/api/v10"

type DiscordSource struct {
	BaseURL   string
	Token     string
	ChannelID string
	Limit     int
	Client    *http.Client
}

func (s DiscordSource) Fetch(ctx context.Context) ([]SourceMessage, error) {
	if s.Token == "" {
		return nil, fmt.Errorf("discord token is required")
	}
	if s.ChannelID == "" {
		return nil, fmt.Errorf("discord channel id is required")
	}
	baseURL := s.BaseURL
	if baseURL == "" {
		baseURL = defaultDiscordBaseURL
	}
	limit := s.Limit
	if limit <= 0 {
		limit = 50
	}
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}

	endpoint, err := url.Parse(baseURL + "/channels/" + url.PathEscape(s.ChannelID) + "/messages")
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	query.Set("limit", strconv.Itoa(limit))
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bot "+s.Token)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkHTTPResponse(resp); err != nil {
		return nil, err
	}

	var payload []discordMessage
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	messages := make([]SourceMessage, 0, len(payload))
	for i := len(payload) - 1; i >= 0; i-- {
		msg := payload[i]
		source := SourceMessage{
			Platform:  "discord",
			ChannelID: s.ChannelID,
			MessageID: msg.ID,
			AuthorID:  msg.Author.ID,
			CreatedAt: msg.Timestamp,
			Content:   msg.Content,
		}
		for _, attachment := range msg.Attachments {
			source.Attachments = append(source.Attachments, Attachment{
				ID:   attachment.ID,
				URL:  attachment.URL,
				Type: attachment.ContentType,
			})
		}
		messages = append(messages, source)
	}
	return messages, nil
}

type DiscordSender struct {
	BaseURL   string
	Token     string
	ChannelID string
	Client    *http.Client
}

func (s DiscordSender) Send(ctx context.Context, output OutputJob) error {
	if s.Token == "" {
		return fmt.Errorf("discord token is required")
	}
	if s.ChannelID == "" {
		return fmt.Errorf("discord channel id is required")
	}
	baseURL := s.BaseURL
	if baseURL == "" {
		baseURL = defaultDiscordBaseURL
	}
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	body, err := json.Marshal(map[string]string{"content": output.Text})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/channels/"+url.PathEscape(s.ChannelID)+"/messages", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+s.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return checkHTTPResponse(resp)
}

type discordMessage struct {
	ID          string              `json:"id"`
	Content     string              `json:"content"`
	Timestamp   string              `json:"timestamp"`
	Author      discordAuthor       `json:"author"`
	Attachments []discordAttachment `json:"attachments"`
}

type discordAuthor struct {
	ID string `json:"id"`
}

type discordAttachment struct {
	ID          string `json:"id"`
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
}
