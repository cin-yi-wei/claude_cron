package channelagent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSetWebhookPostsURLAndSecret(t *testing.T) {
	var gotPath string
	var gotBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	err := SetWebhook(context.Background(), server.URL, "tok123", "https://pub.example/tg/42", "s3cr3t", server.Client())
	if err != nil {
		t.Fatalf("SetWebhook: %v", err)
	}
	if !strings.HasSuffix(gotPath, "/bottok123/setWebhook") {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody["url"] != "https://pub.example/tg/42" || gotBody["secret_token"] != "s3cr3t" {
		t.Fatalf("body = %#v", gotBody)
	}
}

func TestSetWebhookRequiresTokenAndURL(t *testing.T) {
	if err := SetWebhook(context.Background(), "", "", "https://x/y", "", nil); err == nil {
		t.Fatal("want error for missing token")
	}
	if err := SetWebhook(context.Background(), "", "tok", "", "", nil); err == nil {
		t.Fatal("want error for missing url")
	}
}
