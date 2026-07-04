package channelagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Paste-code re-login: when a binding's creds are genuinely expired (a restart
// can't fix it), the supervisor relays the OAuth login URL to the binding's
// channel and asks the user to paste the resulting code back as `code: <value>`.
// This side-route consumes that reply out-of-band and types the code into the
// session's "Paste code here" prompt — completing auth without a human SSH'ing
// into the box. See PASTE_CODE_RELOGIN_SPEC.md.

func reloginPendingDir(root string) string  { return pathIn(root, "relogin", "pending") }
func reloginPendingPath(root string) string { return pathIn(reloginPendingDir(root), "request.json") }

// reloginRequest is the persisted pending re-login (one per binding at a time).
type reloginRequest struct {
	URL       string `json:"url"`
	CreatedAt string `json:"created_at"`
}

// loginCodeRE matches a user reply carrying the OAuth code: "code: XXddd#state".
// The code is required to be a single token with no spaces; the `code:` prefix
// (any case, optional spaces) disambiguates it from ordinary chat.
var loginCodeRE = regexp.MustCompile(`(?i)^\s*code\s*[:：]\s*(\S+)\s*$`)

// parseLoginCode extracts the OAuth code from a user reply, ok=false if the
// message is not a `code:` reply (so it is treated as a normal message).
func parseLoginCode(content string) (code string, ok bool) {
	m := loginCodeRE.FindStringSubmatch(content)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// reloginPostedMu debounces the channel prompt so a persistent login screen
// doesn't repost the URL every supervisor cycle.
var (
	reloginPostedMu  sync.Mutex
	reloginPostedURL = map[string]string{} // binding -> last URL posted
)

// recordReloginRequest persists a pending re-login for root and posts the URL
// prompt to the binding's channel (via its outbox), debounced per binding+URL so
// the same URL isn't reposted. Returns true if it posted this cycle.
func recordReloginRequest(root, binding, url string) bool {
	reloginPostedMu.Lock()
	if reloginPostedURL[binding] == url {
		reloginPostedMu.Unlock()
		return false
	}
	reloginPostedURL[binding] = url
	reloginPostedMu.Unlock()

	_ = AtomicWriteJSON(reloginPendingPath(root), reloginRequest{URL: url, CreatedAt: time.Now().UTC().Format(time.RFC3339)})
	msg := fmt.Sprintf("🔐 `%s` 需要重新登入。開這個網址完成登入,再把畫面給你的 code 貼回來(格式:`code: 貼上的碼`):\n%s", binding, url)
	_ = AtomicWriteJSON(pathIn(root, "outbox", "pending", "relogin-prompt.json"),
		OutputJob{Schema: 1, JobID: "relogin-prompt", Send: true, Text: msg})
	return true
}

// hasPendingRelogin reports whether root has an outstanding re-login request.
func hasPendingRelogin(root string) bool {
	_, err := os.Stat(reloginPendingPath(root))
	return err == nil
}

// clearReloginRequest removes the pending re-login state (on success or giveup)
// so a stale URL doesn't linger.
func clearReloginRequest(root, binding string) {
	_ = os.Remove(reloginPendingPath(root))
	reloginPostedMu.Lock()
	delete(reloginPostedURL, binding)
	reloginPostedMu.Unlock()
}

// loginPaster is an optional Injector capability: type an OAuth code into the
// session's "Paste code here" prompt.
type loginPaster interface {
	PasteLoginCode(ctx context.Context, code string) error
}

// loginTyper is an optional Injector capability: type `/login` to summon the
// OAuth flow (so the login URL appears on a bare "Please run /login" screen).
type loginTyper interface {
	SendLogin(ctx context.Context) error
}

// loginMethodSelector is an optional Injector capability: pick option 1 (Claude
// subscription) on the "Select login method" menu that /login shows before the URL.
type loginMethodSelector interface {
	SelectLoginSubscription(ctx context.Context) error
}

// loginTypeCooldown rate-limits auto-typing /login per binding, so a login screen
// that /login does NOT immediately clear can't make us spam it every cycle.
const loginTypeCooldown = 90 * time.Second

var (
	loginTypedMu sync.Mutex
	loginTypedAt = map[string]time.Time{}
)

// autoLoginAllowed returns true at most once per loginTypeCooldown per binding.
func autoLoginAllowed(binding string) bool {
	loginTypedMu.Lock()
	defer loginTypedMu.Unlock()
	if last, seen := loginTypedAt[binding]; seen && time.Since(last) < loginTypeCooldown {
		return false
	}
	loginTypedAt[binding] = time.Now()
	return true
}

// ResolvePendingReloginOnce consumes a `code: <value>` reply when a re-login is
// pending and types the code into the session. Mirrors ResolvePendingDecisionOnce
// (out-of-band, before the worker takes claude.lock). No-op unless a re-login is
// pending AND the oldest inbox message is a code reply. Returns true if it
// consumed a message.
func ResolvePendingReloginOnce(root string, injector Injector) (bool, error) {
	if err := Init(root); err != nil {
		return false, err
	}
	if !hasPendingRelogin(root) {
		return false, nil
	}
	paster, ok := injector.(loginPaster)
	if !ok {
		return false, nil // injector can't paste (e.g. tests with a stub) → leave it
	}
	// Scan ALL pending inbox messages for the first `code: <value>` reply — NOT
	// just the oldest. If an unrelated message is queued ahead of the code (the
	// user chatted before pasting), checking only the oldest would never reach the
	// code and the re-login would hang forever.
	pendingDir := pathIn(root, "inbox", "pending")
	entries, err := os.ReadDir(pendingDir)
	if err != nil {
		return false, nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // job ids are timestamp-prefixed → chronological
	for _, n := range names {
		p := filepath.Join(pendingDir, n)
		var job InputJob
		if err := ReadJSON(p, &job); err != nil {
			continue
		}
		code, ok := parseLoginCode(job.Source.Content)
		if !ok {
			continue // not a code reply → leave it for normal handling
		}
		if err := paster.PasteLoginCode(context.Background(), code); err != nil {
			return false, err
		}
		// Clear pending + archive the reply so the code isn't re-typed later. The
		// supervisor re-checks the pane next cycle; if still login it re-posts.
		clearReloginRequest(root, bindingNameForRoot(root))
		_ = moveFile(p, pathIn(root, "inbox", "done", n))
		return true, nil
	}
	return false, nil
}

// bindingNameForRoot derives the binding name from its root path (…/bindings/<name>
// or …/control-<name>); best-effort, only used to clear the debounce key.
func bindingNameForRoot(root string) string {
	base := filepath.Base(strings.TrimRight(root, "/"))
	return base
}

// reloginExpiry bounds how long a pending re-login stays open before it's
// considered stale (the URL/code expires; don't leave it wedging the binding).
const reloginExpiry = 15 * time.Minute

// reloginRequestStale reports whether the pending request (if any) is older than
// reloginExpiry, so the supervisor can clear + re-issue a fresh URL.
func reloginRequestStale(root string) bool {
	var r reloginRequest
	if err := ReadJSON(reloginPendingPath(root), &r); err != nil {
		return false
	}
	t, err := time.Parse(time.RFC3339, r.CreatedAt)
	if err != nil {
		return false
	}
	return time.Since(t) > reloginExpiry
}
