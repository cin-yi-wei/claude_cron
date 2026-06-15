package channelagent

import (
	"os"
	"path/filepath"
)

func Init(root string) error {
	for _, dir := range []string{
		"mock",
		"inbox/pending",
		"inbox/processing",
		"inbox/done",
		"inbox/failed",
		"outbox/pending",
		"outbox/sent",
		"outbox/failed",
		"state",
		"locks",
		"logs",
	} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return err
		}
	}
	return nil
}

func pathIn(root string, parts ...string) string {
	all := append([]string{root}, parts...)
	return filepath.Join(all...)
}
