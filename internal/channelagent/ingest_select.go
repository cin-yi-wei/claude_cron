package channelagent

import "fmt"

// bindingTokens carries the resolved bot tokens for each platform so the
// selectors don't reach into the environment themselves (keeps them testable).
type bindingTokens struct {
	discord  string
	telegram string
}

// SelectIngester builds the Ingester for a binding from its platform + mode.
//
//   - poll  → PollIngester wrapping the platform's MessageSource (the existing
//     passive behavior).
//   - push  → not wired yet (Discord Gateway websocket / Telegram webhook are
//     steps 4-5); returns an error so misconfiguration is loud, not silent.
func SelectIngester(b Binding, cfg Config, tokens bindingTokens) (Ingester, error) {
	switch b.ModeOf() {
	case ModePoll:
		source, err := selectSource(b, cfg, tokens)
		if err != nil {
			return nil, err
		}
		return PollIngester{Source: source}, nil
	case ModePush:
		return nil, fmt.Errorf("binding %q: push mode for %s not implemented yet", b.Name, b.PlatformOf())
	default:
		return nil, fmt.Errorf("binding %q: unknown mode %q", b.Name, b.Mode)
	}
}

func selectSource(b Binding, cfg Config, tokens bindingTokens) (MessageSource, error) {
	switch b.PlatformOf() {
	case PlatformDiscord:
		return DiscordSource{BaseURL: cfg.Discord.BaseURL, Token: tokens.discord, ChannelID: b.ChannelID, Limit: 50}, nil
	case PlatformTelegram:
		return TelegramSource{BaseURL: cfg.Telegram.BaseURL, Token: tokens.telegram, ChatID: b.ChannelID}, nil
	default:
		return nil, fmt.Errorf("binding %q: unknown platform %q", b.Name, b.Platform)
	}
}

// SelectSender builds the reply Sender for a binding from its platform.
func SelectSender(b Binding, cfg Config, tokens bindingTokens) (Sender, error) {
	switch b.PlatformOf() {
	case PlatformDiscord:
		return DiscordSender{BaseURL: cfg.Discord.BaseURL, Token: tokens.discord, ChannelID: b.ChannelID}, nil
	case PlatformTelegram:
		return TelegramSender{BaseURL: cfg.Telegram.BaseURL, Token: tokens.telegram, ChatID: b.ChannelID}, nil
	default:
		return nil, fmt.Errorf("binding %q: unknown platform %q", b.Name, b.Platform)
	}
}
