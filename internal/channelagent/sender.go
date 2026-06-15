package channelagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type Sender interface {
	Send(ctx context.Context, output OutputJob) error
}

type outHashState struct {
	Hashes map[string]string `json:"hashes"`
}

func RunSenderOnce(ctx context.Context, root string, sender Sender) (int, error) {
	if err := Init(root); err != nil {
		return 0, err
	}
	lock, err := AcquireLock(pathIn(root, "locks", "sender.lock"))
	if err != nil {
		return 0, err
	}
	defer lock.Release()

	statePath := pathIn(root, "state", "last_out_hashes.json")
	state, err := readOutHashState(statePath)
	if err != nil {
		return 0, err
	}

	sentCount := 0
	for {
		pendingPath, err := oldestJSON(pathIn(root, "outbox", "pending"))
		if err != nil {
			return sentCount, err
		}
		if pendingPath == "" {
			break
		}
		name := filepath.Base(pendingPath)
		var output OutputJob
		if err := ReadJSON(pendingPath, &output); err != nil {
			_ = moveFile(pendingPath, pathIn(root, "outbox", "failed", name))
			return sentCount, err
		}
		hash, err := HashOutput(output)
		if err != nil {
			_ = moveFile(pendingPath, pathIn(root, "outbox", "failed", name))
			return sentCount, err
		}
		if state.Hashes[output.JobID] == hash {
			if err := moveFile(pendingPath, pathIn(root, "outbox", "sent", name)); err != nil {
				return sentCount, err
			}
			continue
		}
		if output.Send {
			if sender == nil {
				_ = moveFile(pendingPath, pathIn(root, "outbox", "failed", name))
				return sentCount, errors.New("sender is nil")
			}
			if err := sender.Send(ctx, output); err != nil {
				_ = moveFile(pendingPath, pathIn(root, "outbox", "failed", name))
				return sentCount, err
			}
			sentCount++
		}
		state.Hashes[output.JobID] = hash
		if err := writeOutHashState(statePath, state); err != nil {
			return sentCount, err
		}
		if err := moveFile(pendingPath, pathIn(root, "outbox", "sent", name)); err != nil {
			return sentCount, err
		}
	}
	return sentCount, nil
}

func readOutHashState(path string) (outHashState, error) {
	state := outHashState{Hashes: map[string]string{}}
	if err := ReadJSON(path, &state); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return state, nil
		}
		return state, err
	}
	if state.Hashes == nil {
		state.Hashes = map[string]string{}
	}
	return state, nil
}

func writeOutHashState(path string, state outHashState) error {
	if state.Hashes == nil {
		return fmt.Errorf("out hash state has nil hashes")
	}
	return AtomicWriteJSON(path, state)
}
