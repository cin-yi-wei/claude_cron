package channelagent

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Admin API for scheduled triggers（定時任務）：/api/triggers。跟 registry 一樣用
// 檔案鎖序列化寫入（scheduler 的 ticker 也會寫 triggers.json，避免互相蓋掉）。

type adminTriggerRequest struct {
	Name     string `json:"name"`
	Binding  string `json:"binding"`
	Cron     string `json:"cron"`
	Timezone string `json:"timezone"`
	Message  string `json:"message"`
	CatchUp  *bool  `json:"catch_up"`
}

func (h AdminHandler) listTriggers(w http.ResponseWriter) {
	cfg, err := LoadTriggers(h.Root)
	if err != nil {
		http.Error(w, "triggers error", http.StatusInternalServerError)
		return
	}
	out := make([]Trigger, 0, len(cfg.Triggers))
	out = append(out, cfg.Triggers...)
	writeJSONResponse(w, out)
}

func (h AdminHandler) createTrigger(w http.ResponseWriter, r *http.Request) {
	var req adminTriggerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !validBindingName(req.Name) {
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	}
	if _, err := ParseCron(req.Cron); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	tz := req.Timezone
	if tz == "" {
		tz = DefaultTriggerTimezone
	}
	if _, err := time.LoadLocation(tz); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.Message == "" {
		http.Error(w, "message required", http.StatusBadRequest)
		return
	}

	lock, err := AcquireLock(pathIn(h.Root, "locks", "triggers.lock"))
	if err != nil {
		http.Error(w, "lock error", http.StatusInternalServerError)
		return
	}
	defer lock.Release()

	reg, err := LoadRegistry(h.Root)
	if err != nil {
		http.Error(w, "registry error", http.StatusInternalServerError)
		return
	}
	if _, ok := reg.Get(req.Binding); !ok {
		http.Error(w, "binding not found", http.StatusBadRequest)
		return
	}

	cfg, err := LoadTriggers(h.Root)
	if err != nil {
		http.Error(w, "triggers error", http.StatusInternalServerError)
		return
	}
	catchUp := true
	if req.CatchUp != nil {
		catchUp = *req.CatchUp
	}
	now := time.Now().UTC().Format(time.RFC3339)
	t := Trigger{
		Name:        req.Name,
		Binding:     req.Binding,
		Cron:        req.Cron,
		Timezone:    tz,
		Message:     req.Message,
		CatchUp:     catchUp,
		Enabled:     true,
		CreatedAt:   now,
		LastChecked: now, // 從建立時間開始算到期，不回頭補跑建立前的時間點
	}
	if err := cfg.Add(t); err != nil {
		writeJSONStatus(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if err := SaveTriggers(h.Root, cfg); err != nil {
		http.Error(w, "save error", http.StatusInternalServerError)
		return
	}
	writeJSONStatus(w, http.StatusCreated, t)
}

func (h AdminHandler) deleteTrigger(w http.ResponseWriter, name string) {
	if !validBindingName(name) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	lock, err := AcquireLock(pathIn(h.Root, "locks", "triggers.lock"))
	if err != nil {
		http.Error(w, "lock error", http.StatusInternalServerError)
		return
	}
	defer lock.Release()

	cfg, err := LoadTriggers(h.Root)
	if err != nil {
		http.Error(w, "triggers error", http.StatusInternalServerError)
		return
	}
	if !cfg.Remove(name) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := SaveTriggers(h.Root, cfg); err != nil {
		http.Error(w, "save error", http.StatusInternalServerError)
		return
	}
	writeJSONResponse(w, map[string]string{"result": "removed " + name})
}

// setTriggerEnabled handles POST /api/triggers/{name}/enable|disable.
func (h AdminHandler) setTriggerEnabled(w http.ResponseWriter, r *http.Request, name string, enabled bool) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !validBindingName(name) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	lock, err := AcquireLock(pathIn(h.Root, "locks", "triggers.lock"))
	if err != nil {
		http.Error(w, "lock error", http.StatusInternalServerError)
		return
	}
	defer lock.Release()

	cfg, err := LoadTriggers(h.Root)
	if err != nil {
		http.Error(w, "triggers error", http.StatusInternalServerError)
		return
	}
	found := false
	for i := range cfg.Triggers {
		if cfg.Triggers[i].Name == name {
			cfg.Triggers[i].Enabled = enabled
			found = true
			break
		}
	}
	if !found {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := SaveTriggers(h.Root, cfg); err != nil {
		http.Error(w, "save error", http.StatusInternalServerError)
		return
	}
	verb := "disabled"
	if enabled {
		verb = "enabled"
	}
	writeJSONResponse(w, map[string]string{"result": name + " " + verb})
}

// testTrigger handles POST /api/triggers/{name}/test — manually fires the
// trigger's message into its binding's inbox right now (bypasses cron timing;
// useful for verifying a new trigger before waiting for its schedule).
func (h AdminHandler) testTrigger(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !validBindingName(name) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	cfg, err := LoadTriggers(h.Root)
	if err != nil {
		http.Error(w, "triggers error", http.StatusInternalServerError)
		return
	}
	t, ok := cfg.Get(name)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	reg, err := LoadRegistry(h.Root)
	if err != nil {
		http.Error(w, "registry error", http.StatusInternalServerError)
		return
	}
	binding, ok := reg.Get(t.Binding)
	if !ok {
		http.Error(w, "binding not found", http.StatusBadRequest)
		return
	}
	msg := BuildTriggerMessage(t, binding, time.Now())
	if _, err := IngestMessages(context.Background(), binding.Root, []SourceMessage{msg}); err != nil {
		writeJSONStatus(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSONResponse(w, map[string]string{"result": "fired " + name + " into " + t.Binding})
}
