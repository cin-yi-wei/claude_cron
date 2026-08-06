package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedA2ARoot 造一個指向 fake admin server 的 root。
func seedA2ARoot(t *testing.T, adminAddr string) string {
	t.Helper()
	root := t.TempDir()
	blob, _ := json.Marshal(map[string]any{
		"admin": map[string]any{"listen": adminAddr, "token": "adm-token"},
		"a2a":   map[string]any{"enabled": true, "listen": "127.0.0.1:8790"},
	})
	if err := os.WriteFile(filepath.Join(root, "config.json"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// CLI 是 admin API 的薄客戶端。它自己寫檔會打破「只有 serve 寫這些檔」這個
// 不變量（a2a_store.go:10 的 in-process mutex 就是靠它成立的）。
func TestA2ACLIGoesThroughTheAdminAPI(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.Method + " " + r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"name":"pm"}`))
	}))
	defer srv.Close()
	root := seedA2ARoot(t, strings.TrimPrefix(srv.URL, "http://"))

	var out, errOut bytes.Buffer
	code := runA2ACommand([]string{"agent", "add", "pm", "--project=/p/pm", "--enabled", "--root", root}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if gotPath != "POST /api/a2a/agents" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer adm-token" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"name":"pm"`) || !strings.Contains(gotBody, `"project_dir":"/p/pm"`) {
		t.Fatalf("body = %s", gotBody)
	}
	if _, err := os.Stat(filepath.Join(root, "agents.json")); err == nil {
		t.Fatal("the online path must not write agents.json directly")
	}
}

// --offline 必須先探 /api/healthz，探得到就拒絕執行。
func TestA2ACLIOfflineRefusesWhileServeIsUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/healthz" {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	root := seedA2ARoot(t, strings.TrimPrefix(srv.URL, "http://"))

	var out, errOut bytes.Buffer
	code := runA2ACommand([]string{"agent", "list", "--offline", "--root", root}, &out, &errOut)
	if code == 0 {
		t.Fatal("--offline must refuse while serve is reachable")
	}
	if !strings.Contains(errOut.String(), "serve") {
		t.Fatalf("the refusal must say why: %s", errOut.String())
	}
}

func TestA2ACLIOfflineWritesWhenServeIsDown(t *testing.T) {
	// 127.0.0.1:1 沒有東西在聽。
	root := seedA2ARoot(t, "127.0.0.1:1")
	var out, errOut bytes.Buffer
	code := runA2ACommand([]string{"agent", "add", "pm", "--project=/p/pm", "--enabled", "--offline", "--root", root}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(root, "agents.json")); err != nil {
		t.Fatalf("offline mode must write agents.json: %v", err)
	}
}

func TestA2ACLIRejectsUnknownVerb(t *testing.T) {
	root := seedA2ARoot(t, "127.0.0.1:1")
	var out, errOut bytes.Buffer
	if code := runA2ACommand([]string{"agent", "frobnicate", "x", "--root", root}, &out, &errOut); code == 0 {
		t.Fatal("an unknown verb must exit non-zero")
	}
}
