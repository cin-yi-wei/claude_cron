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
