package channelagent

import "testing"

func testCfg() Config {
	return Config{
		Discord:  DiscordConfig{BaseURL: "https://dc.test", TokenEnv: "DISCORD_BOT_TOKEN"},
		Telegram: TelegramConfig{BaseURL: "https://tg.test", TokenEnv: "TELEGRAM_BOT_TOKEN"},
	}
}

func TestSelectIngesterDiscordPoll(t *testing.T) {
	b := Binding{Name: "x", ChannelID: "c1"} // empty platform/mode → discord/poll
	ing, err := SelectIngester(b, testCfg(), bindingTokens{discord: "dtok"})
	if err != nil {
		t.Fatalf("SelectIngester: %v", err)
	}
	poll, ok := ing.(PollIngester)
	if !ok {
		t.Fatalf("want PollIngester, got %T", ing)
	}
	src, ok := poll.Source.(DiscordSource)
	if !ok {
		t.Fatalf("want DiscordSource, got %T", poll.Source)
	}
	if src.ChannelID != "c1" || src.Token != "dtok" {
		t.Fatalf("source = %#v", src)
	}
}

func TestSelectIngesterTelegramPoll(t *testing.T) {
	b := Binding{Name: "y", ChannelID: "12345", Platform: PlatformTelegram, Mode: ModePoll}
	ing, err := SelectIngester(b, testCfg(), bindingTokens{telegram: "ttok"})
	if err != nil {
		t.Fatalf("SelectIngester: %v", err)
	}
	src := ing.(PollIngester).Source.(TelegramSource)
	if src.ChatID != "12345" || src.Token != "ttok" {
		t.Fatalf("source = %#v", src)
	}
}

func TestSelectIngesterPushNotImplemented(t *testing.T) {
	b := Binding{Name: "z", ChannelID: "c1", Mode: ModePush}
	if _, err := SelectIngester(b, testCfg(), bindingTokens{}); err == nil {
		t.Fatal("want error for push mode, got nil")
	}
}

func TestSelectSenderByPlatform(t *testing.T) {
	dc, err := SelectSender(Binding{ChannelID: "c1"}, testCfg(), bindingTokens{discord: "d"})
	if err != nil {
		t.Fatalf("SelectSender dc: %v", err)
	}
	if _, ok := dc.(DiscordSender); !ok {
		t.Fatalf("want DiscordSender, got %T", dc)
	}
	tg, err := SelectSender(Binding{ChannelID: "9", Platform: PlatformTelegram}, testCfg(), bindingTokens{telegram: "t"})
	if err != nil {
		t.Fatalf("SelectSender tg: %v", err)
	}
	if _, ok := tg.(TelegramSender); !ok {
		t.Fatalf("want TelegramSender, got %T", tg)
	}
}

func TestParseCommandOpts(t *testing.T) {
	cmd, ok := ParseCommand("/bind foo /dir main --platform=tg --mode=push --chat-id=42 --delete-channel")
	if !ok {
		t.Fatal("ParseCommand ok=false")
	}
	if cmd.opt("platform") != "tg" || cmd.opt("mode") != "push" || cmd.opt("chat-id") != "42" {
		t.Fatalf("opts = %#v", cmd.Opts)
	}
	if !cmd.Flags["delete-channel"] {
		t.Fatal("delete-channel flag not set")
	}
	if len(cmd.Args) != 3 {
		t.Fatalf("args = %#v", cmd.Args)
	}
}

func TestNormalizePlatformAndMode(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{{"", PlatformDiscord}, {"dc", PlatformDiscord}, {"telegram", PlatformTelegram}, {"tg", PlatformTelegram}}
	for _, c := range cases {
		got, err := normalizePlatform(c.in)
		if err != nil || got != c.want {
			t.Fatalf("normalizePlatform(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	if _, err := normalizePlatform("slack"); err == nil {
		t.Fatal("want error for unknown platform")
	}
	if got, _ := normalizeMode(""); got != ModePoll {
		t.Fatalf("normalizeMode(\"\") = %q, want poll", got)
	}
	if got, _ := normalizeMode("active"); got != ModePush {
		t.Fatalf("normalizeMode(active) = %q, want push", got)
	}
}
