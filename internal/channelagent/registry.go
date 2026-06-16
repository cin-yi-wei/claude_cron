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
}

// Platform and Mode values. Empty string is treated as the default
// (PlatformDiscord / ModePoll) for backward compatibility with older registries.
const (
	PlatformDiscord  = "discord"
	PlatformTelegram = "telegram"
	ModePoll         = "poll"
	ModePush         = "push"
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

type Registry struct {
	Bindings []Binding `json:"bindings"`
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
		Worktree:    filepath.Join(root, "worktrees", name),
		TmuxSession: "cc-" + name,
		Root:        filepath.Join(root, "bindings", name),
	}
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
