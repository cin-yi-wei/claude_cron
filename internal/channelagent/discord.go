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

// httpClient15s is a shared bounded client for outbound channel sends, so a hung
// connection can't stall the (sequential) activity ticker for every binding.
var httpClient15s = &http.Client{Timeout: 15 * time.Second}

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
		client = httpClient15s // bounded: a hung send must not stall the activity ticker
	}
	// discordColorDiff rewrites ```diff→```ansi, injecting colour codes that can
	// push the content past Discord's 2000-char hard limit. Chunk the FINAL text
	// so every POST is under the limit (the upstream splitter is colour-blind).
	for _, content := range chunkDiscord(discordColorDiff(output.Text), 2000) {
		body, err := json.Marshal(map[string]string{"content": content})
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
		err = checkHTTPResponse(resp)
		resp.Body.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// chunkDiscord splits content into pieces ≤ max runes, breaking on line
// boundaries and reopening a ```lang fence that a split leaves open so colour
// rendering stays valid across pieces. Content already under the limit is
// returned as-is (single element).
func chunkDiscord(content string, max int) []string {
	if len([]rune(content)) <= max {
		return []string{content}
	}
	var out []string
	var b strings.Builder
	openFence := "" // e.g. "```ansi" while inside a fenced block
	reopen := ""
	flush := func() {
		s := b.String()
		if strings.Count(s, "```")%2 == 1 {
			s += "\n```" // close a fence left open by the cut
			reopen = openFence
		} else {
			reopen = ""
		}
		out = append(out, s)
		b.Reset()
	}
	for _, ln := range strings.Split(content, "\n") {
		// Track fence state from this line before deciding to cut.
		add := ln
		if reopen != "" {
			add = reopen + "\n" + ln
			reopen = ""
		}
		if b.Len() > 0 && len([]rune(b.String()))+1+len([]rune(add)) > max {
			flush()
			if reopen != "" {
				add = reopen + "\n" + ln
				reopen = ""
			} else {
				add = ln
			}
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(add)
		// Update fence tracking: a line that is exactly a fence open/close toggles.
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			if openFence == "" {
				openFence = strings.TrimSpace(ln)
			} else {
				openFence = ""
			}
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
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
