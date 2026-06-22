package channelagent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

type Binding struct {
	Name        string `json:"name"`
	ChannelID   string `json:"channel_id"`
	ProjectDir  string `json:"project_dir"`
	Branch      string `json:"branch"`
	Worktree    string `json:"worktree"`
	TmuxSession string `json:"tmux_session"`
	Root        string `json:"root"`
	CreatedAt   string `json:"created_at"`
	// Platform is "discord" or "telegram"; Mode is "poll" (passive) or "push"
	// (active). Both are optional in stored JSON: an empty value means the
	// legacy default (discord/poll), so existing registries keep working.
	Platform string `json:"platform,omitempty"`
	Mode     string `json:"mode,omitempty"`
	// Paused marks a binding as "hot-stopped": its tmux session is killed to free
	// memory but the binding + worktree + transcript are kept. The supervisor skips
	// starting the session and ingesting messages while paused; /resume clears it
	// and the next cycle recreates the session (auto-resuming the transcript).
	Paused bool `json:"paused,omitempty"`
	// Plane is the control plane that owns this binding (e.g. "discord" or
	// "telegram"). Each control plane only sees/manages its own bindings. Empty
	// means the legacy default plane ("discord"), so existing registries keep
	// working. Names remain globally unique across planes.
	Plane string `json:"plane,omitempty"`
	// Control marks this binding as a control plane (the management assistant)
	// rather than a worker. Control bindings have no project worktree/branch; the
	// supervisor drives them through the control pipeline (RunControlOnce + the
	// assistant session) instead of the worker pipeline.
	Control bool `json:"control,omitempty"`
	// Default marks the protected bootstrap control plane: the first control
	// created becomes the default, and it cannot be paused/unbound (that would
	// lock the user out of all management). The flag is transferable to another
	// control via set-default.
	Default bool `json:"default,omitempty"`
	// Sleeping marks a binding auto-slept after being idle: its session is killed
	// to free RAM (like Paused) BUT it auto-wakes when a new message arrives.
	// Distinct from Paused (manual; stays down + ignores messages until /resume).
	Sleeping bool `json:"sleeping,omitempty"`
	// AutoApprove makes the permission gate auto-allow every gated tool for this
	// binding (no channel y/n) — a trusted-binding "bypass" mode, off by default.
	AutoApprove bool `json:"auto_approve,omitempty"`
}

// ControlBindingDefaults derives a control binding's session/root/workspace.
// Control bindings have no project worktree (no dir/branch); the workspace is a
// plain sandbox dir under controls/<name>.
func ControlBindingDefaults(root, name, platform string) Binding {
	base := filepath.Join(root, "controls", name)
	return Binding{
		Name:        name,
		Platform:    platform,
		Control:     true,
		TmuxSession: "cc-" + name,
		Root:        base,
		Worktree:    filepath.Join(base, "workspace"),
	}
}

// HasControl reports whether any control binding exists (used to decide if a
// newly-created control becomes the protected default).
func (r Registry) HasControl() bool {
	for _, b := range r.Bindings {
		if b.Control {
			return true
		}
	}
	return false
}

// PlaneOf returns the owning control plane, defaulting to "discord" when unset.
func (b Binding) PlaneOf() string {
	if b.Plane == "" {
		return PlatformDiscord
	}
	return b.Plane
}

// Platform and Mode values. Empty string is treated as the default
// (PlatformDiscord / ModePoll) for backward compatibility with older registries.
const (
	PlatformDiscord  = "discord"
	PlatformTelegram = "telegram"
	// PlatformWeb is the in-browser chat platform: no external channel/chat —
	// messages arrive via the admin SSE/POST endpoints and replies are delivered
	// to connected browsers through the in-process ChatHub. Its ChannelID is the
	// binding name (there is no upstream channel to provision).
	PlatformWeb = "web"
	ModePoll    = "poll"
	ModePush    = "push"
)

// PlatformOf returns the binding's platform, defaulting to discord when unset.
func (b Binding) PlatformOf() string {
	if b.Platform == "" {
		return PlatformDiscord
	}
	return b.Platform
}

// ModeOf returns the binding's arrival mode, defaulting to poll when unset.
func (b Binding) ModeOf() string {
	if b.Mode == "" {
		return ModePoll
	}
	return b.Mode
}

// UnboundChannel is a tombstone left when a Discord worker binding is unbound:
// the channel is kept (we never delete channels) but no longer has a session.
// If a message later lands in such a channel (seen via the Gateway demux), the
// supervisor pings the control channel offering to rebind. Cleared on rebind.
type UnboundChannel struct {
	Name       string `json:"name"`
	ChannelID  string `json:"channel_id"`
	ProjectDir string `json:"project_dir"`
	Branch     string `json:"branch"`
	Plane      string `json:"plane,omitempty"`
}

type Registry struct {
	Bindings []Binding        `json:"bindings"`
	Unbound  []UnboundChannel `json:"unbound,omitempty"`
}

// AddUnbound records (or refreshes) a tombstone, keyed by channel id.
func (r *Registry) AddUnbound(u UnboundChannel) {
	for i := range r.Unbound {
		if r.Unbound[i].ChannelID == u.ChannelID {
			r.Unbound[i] = u
			return
		}
	}
	r.Unbound = append(r.Unbound, u)
}

// UnboundByChannel returns the tombstone for a channel id, if any.
func (r *Registry) UnboundByChannel(channelID string) (UnboundChannel, bool) {
	for _, u := range r.Unbound {
		if u.ChannelID == channelID {
			return u, true
		}
	}
	return UnboundChannel{}, false
}

// UnboundByName returns the tombstone for a binding name, if any.
func (r *Registry) UnboundByName(name string) (UnboundChannel, bool) {
	for _, u := range r.Unbound {
		if u.Name == name {
			return u, true
		}
	}
	return UnboundChannel{}, false
}

// RemoveUnboundByName drops a tombstone by binding name (called on rebind).
func (r *Registry) RemoveUnboundByName(name string) bool {
	for i := range r.Unbound {
		if r.Unbound[i].Name == name {
			r.Unbound = append(r.Unbound[:i], r.Unbound[i+1:]...)
			return true
		}
	}
	return false
}

var validNameRE = regexp.MustCompile(`^[a-z0-9-]+$`)

func ValidName(name string) bool {
	return validNameRE.MatchString(name)
}

// BindingDefaults derives the session/worktree/root fields from name+root.
// ChannelID and CreatedAt are filled in by the caller after provisioning.
func BindingDefaults(root, name, projectDir, branch string) Binding {
	return Binding{
		Name:        name,
		ProjectDir:  projectDir,
		Branch:      branch,
		Worktree:    WorktreePath(projectDir, name),
		TmuxSession: "cc-" + name,
		Root:        filepath.Join(root, "bindings", name),
	}
}

// WorktreePath places a binding's git worktree as a sibling of the main project
// directory (the conventional layout: project repo and its worktrees live
// side-by-side under the same parent), named after the binding. Runtime state
// (inbox/outbox) still lives under root/bindings/<name>, separate from the code.
func WorktreePath(projectDir, name string) string {
	if abs, err := filepath.Abs(projectDir); err == nil {
		projectDir = abs
	}
	return filepath.Join(filepath.Dir(projectDir), name)
}

func RegistryPath(root string) string {
	return filepath.Join(root, "bindings.json")
}

func LoadRegistry(root string) (Registry, error) {
	var reg Registry
	if err := ReadJSON(RegistryPath(root), &reg); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Registry{}, nil
		}
		return Registry{}, err
	}
	return reg, nil
}

func SaveRegistry(root string, reg Registry) error {
	return AtomicWriteJSON(RegistryPath(root), reg)
}

func (r *Registry) Get(name string) (Binding, bool) {
	for _, b := range r.Bindings {
		if b.Name == name {
			return b, true
		}
	}
	return Binding{}, false
}

func (r *Registry) Add(b Binding) error {
	if _, ok := r.Get(b.Name); ok {
		return fmt.Errorf("binding %q already exists", b.Name)
	}
	r.Bindings = append(r.Bindings, b)
	return nil
}

// SetPaused flips the paused flag on a binding by name. Returns false if no such
// binding exists. The caller persists the registry.
func (r *Registry) SetPaused(name string, paused bool) bool {
	for i := range r.Bindings {
		if r.Bindings[i].Name == name {
			r.Bindings[i].Paused = paused
			return true
		}
	}
	return false
}

// SetDefaultControl makes the named control binding the sole default, clearing
// the flag on every other control. Returns false if name is not a control
// binding. The caller persists the registry.
func (r *Registry) SetDefaultControl(name string) bool {
	idx := -1
	for i := range r.Bindings {
		if r.Bindings[i].Name == name && r.Bindings[i].Control {
			idx = i
		}
	}
	if idx < 0 {
		return false
	}
	for i := range r.Bindings {
		r.Bindings[i].Default = r.Bindings[i].Control && i == idx
	}
	return true
}

// SetSleeping flips the auto-sleep flag on a binding by name. Caller persists.
func (r *Registry) SetSleeping(name string, sleeping bool) bool {
	for i := range r.Bindings {
		if r.Bindings[i].Name == name {
			r.Bindings[i].Sleeping = sleeping
			return true
		}
	}
	return false
}

// SetAutoApprove flips the auto-approve (permission bypass) flag. Caller persists.
func (r *Registry) SetAutoApprove(name string, on bool) bool {
	for i := range r.Bindings {
		if r.Bindings[i].Name == name {
			r.Bindings[i].AutoApprove = on
			return true
		}
	}
	return false
}

func (r *Registry) Remove(name string) bool {
	for i, b := range r.Bindings {
		if b.Name == name {
			r.Bindings = append(r.Bindings[:i], r.Bindings[i+1:]...)
			return true
		}
	}
	return false
}

func (r Registry) Names() []string {
	names := make([]string, 0, len(r.Bindings))
	for _, b := range r.Bindings {
		names = append(names, b.Name)
	}
	return names
}
