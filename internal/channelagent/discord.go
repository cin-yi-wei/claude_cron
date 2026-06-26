package channelagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// httpClient15s is a shared bounded client for outbound channel sends, so a hung
// connection can't stall the (sequential) activity ticker for every binding.
var httpClient15s = &http.Client{Timeout: 15 * time.Second}

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
	// Send the text as-is (plain ```diff fences). We used to rewrite ```diff→```ansi
	// for red/green colour, but the \x1b escape codes leaked as literal "[32m"/"[0m"
	// text whenever a long diff was chunked (the ansi fence split mid-block) — see
	// the broken cards 2026-06-27. Discord's native ```diff highlight has no escape
	// codes to leak, so it survives chunking cleanly. Chunk so every POST is ≤2000.
	for _, content := range chunkDiscord(output.Text, 2000) {
		body, err := json.Marshal(map[string]string{"content": content})
		if err != nil {
			return err
		}
		if err := s.postMessage(ctx, client, baseURL, body); err != nil {
			return err
		}
	}
	return nil
}

// discordMaxSendAttempts bounds the per-message POST retries (one extra try per
// 429 / transient network blip). discordRetryCap clamps how long we'll honour a
// server-supplied retry_after so a pathological value can't wedge the sender.
const (
	discordMaxSendAttempts = 6
	discordRetryCap        = 5 * time.Second
)

// postMessage POSTs one ≤2000-char message, pacing via the per-channel throttle
// and honouring Discord 429 rate limits: on 429 it reads retry_after and waits
// that long before retrying, instead of dropping the message. Before this, the
// un-throttled activity ticker fired bursts of POSTs that 429'd en masse and the
// messages were lost (diff cards vanished, replies delayed). Non-429 HTTP errors
// return immediately (retrying won't help).
func (s DiscordSender) postMessage(ctx context.Context, client *http.Client, baseURL string, body []byte) error {
	var lastErr error
	for attempt := 0; attempt < discordMaxSendAttempts; attempt++ {
		discordThrottle(ctx, s.ChannelID)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/channels/"+url.PathEscape(s.ChannelID)+"/messages", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bot "+s.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			// Transient network blip: brief backoff then retry.
			lastErr = err
			if !sleepCtx(ctx, backoffDelay(attempt)) {
				return ctx.Err()
			}
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			wait := parseRetryAfter(resp)
			resp.Body.Close()
			lastErr = fmt.Errorf("http 429 (retry_after %s)", wait)
			if !sleepCtx(ctx, wait) {
				return ctx.Err()
			}
			continue
		}
		err = checkHTTPResponse(resp)
		resp.Body.Close()
		return err // success (nil) or a non-429 HTTP error that won't improve on retry
	}
	return lastErr
}

// parseRetryAfter extracts how long Discord wants us to wait from a 429 response:
// the JSON body's "retry_after" (seconds, float) first, then the Retry-After
// header. Defaults to 1s and is clamped to discordRetryCap.
func parseRetryAfter(resp *http.Response) time.Duration {
	wait := time.Second
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	var parsed struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.RetryAfter > 0 {
		wait = time.Duration(parsed.RetryAfter * float64(time.Second))
	} else if h := resp.Header.Get("Retry-After"); h != "" {
		if secs, err := strconv.ParseFloat(h, 64); err == nil && secs > 0 {
			wait = time.Duration(secs * float64(time.Second))
		}
	}
	if wait > discordRetryCap {
		wait = discordRetryCap
	}
	return wait
}

// backoffDelay is the wait before retrying a transient network error: 250ms,
// 500ms, 1s, ... capped at discordRetryCap.
func backoffDelay(attempt int) time.Duration {
	d := 250 * time.Millisecond << attempt
	if d > discordRetryCap {
		d = discordRetryCap
	}
	return d
}

// sleepCtx waits d, returning false if ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Per-channel send pacing. Discord's per-channel message limit is ~5/s; the
// activity ticker + reply sender share the same bot token and channel route, so
// without pacing a burst of activity (many diff cards from one turn) blew past it
// and 429'd. discordThrottle spaces POSTs to the same channel by discordMinInterval.
var (
	discordSendMu   sync.Mutex
	discordNextSend = map[string]time.Time{}
)

const discordMinInterval = 250 * time.Millisecond

// discordThrottle blocks until it's safe to POST to channelID, then reserves the
// next slot. Concurrency-safe; the wait happens outside the lock so callers to
// different channels don't serialise on each other.
func discordThrottle(ctx context.Context, channelID string) {
	discordSendMu.Lock()
	now := time.Now()
	next := discordNextSend[channelID]
	if next.Before(now) {
		next = now
	}
	wait := next.Sub(now)
	discordNextSend[channelID] = next.Add(discordMinInterval)
	discordSendMu.Unlock()
	sleepCtx(ctx, wait)
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
