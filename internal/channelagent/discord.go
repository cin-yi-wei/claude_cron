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
)

var discordDiffFenceRE = regexp.MustCompile("(?s)```diff\\n(.*?)```")

// discordColorDiff rewrites ```diff blocks to ```ansi blocks with real ANSI
// colour codes — Discord renders ```ansi reliably (− red, + green), unlike its
// dim/inconsistent ```diff highlighting. Other platforms keep ```diff.
func discordColorDiff(text string) string {
	if !strings.Contains(text, "```diff") {
		return text
	}
	return discordDiffFenceRE.ReplaceAllStringFunc(text, func(m string) string {
		inner := strings.TrimSuffix(strings.TrimPrefix(m, "```diff\n"), "```")
		var b strings.Builder
		b.WriteString("```ansi\n")
		for _, ln := range strings.Split(strings.TrimRight(inner, "\n"), "\n") {
			switch {
			case strings.HasPrefix(ln, "- "):
				b.WriteString("\x1b[31m" + ln + "\x1b[0m\n")
			case strings.HasPrefix(ln, "+ "):
				b.WriteString("\x1b[32m" + ln + "\x1b[0m\n")
			default:
				b.WriteString(ln + "\n")
			}
		}
		b.WriteString("```")
		return b.String()
	})
}

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
		// Skip messages authored by bots (including this bot itself) to avoid
		// echo loops where the agent's own replies are re-ingested as new input.
		if msg.Author.Bot {
			continue
		}
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
	body, err := json.Marshal(map[string]string{"content": discordColorDiff(output.Text)})
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

type DiscordAdmin struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

func (a DiscordAdmin) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}
	return http.DefaultClient
}

func (a DiscordAdmin) baseURL() string {
	if a.BaseURL != "" {
		return a.BaseURL
	}
	return defaultDiscordBaseURL
}

// CreateChannel creates a text channel (type 0) in guildID and returns its id.
func (a DiscordAdmin) CreateChannel(ctx context.Context, guildID, name string) (string, error) {
	if a.Token == "" {
		return "", fmt.Errorf("discord token is required")
	}
	if guildID == "" {
		return "", fmt.Errorf("discord guild id is required")
	}
	body, err := json.Marshal(map[string]any{"name": name, "type": 0})
	if err != nil {
		return "", err
	}
	endpoint := a.baseURL() + "/guilds/" + url.PathEscape(guildID) + "/channels"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bot "+a.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := checkHTTPResponse(resp); err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// DeleteChannel deletes a channel by id.
func (a DiscordAdmin) DeleteChannel(ctx context.Context, channelID string) error {
	if a.Token == "" {
		return fmt.Errorf("discord token is required")
	}
	endpoint := a.baseURL() + "/channels/" + url.PathEscape(channelID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+a.Token)
	resp, err := a.client().Do(req)
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
	ID  string `json:"id"`
	Bot bool   `json:"bot"`
}

type discordAttachment struct {
	ID          string `json:"id"`
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
}
