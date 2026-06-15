package channelagent

import "testing"

func TestCanonicalHashStableForSameSource(t *testing.T) {
	source := SourceMessage{
		Platform:    "mock",
		ChannelID:   "local",
		MessageID:   "msg-1",
		AuthorID:    "user-1",
		CreatedAt:   "2026-06-16T01:30:12+08:00",
		Content:     "hello",
		Attachments: []Attachment{{ID: "a1", URL: "https://example.test/a.png", Type: "image/png"}},
	}

	first, err := HashSource(source)
	if err != nil {
		t.Fatalf("HashSource first: %v", err)
	}
	second, err := HashSource(source)
	if err != nil {
		t.Fatalf("HashSource second: %v", err)
	}

	if first != second {
		t.Fatalf("hash changed for identical source: %s != %s", first, second)
	}
}

func TestCanonicalHashChangesWhenContentChanges(t *testing.T) {
	source := SourceMessage{
		Platform:  "mock",
		ChannelID: "local",
		MessageID: "msg-1",
		AuthorID:  "user-1",
		CreatedAt: "2026-06-16T01:30:12+08:00",
		Content:   "hello",
	}

	first, err := HashSource(source)
	if err != nil {
		t.Fatalf("HashSource first: %v", err)
	}
	source.Content = "hello edited"
	second, err := HashSource(source)
	if err != nil {
		t.Fatalf("HashSource second: %v", err)
	}

	if first == second {
		t.Fatalf("hash did not change after content edit: %s", first)
	}
}
