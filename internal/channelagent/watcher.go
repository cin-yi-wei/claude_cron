package channelagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

type seenState struct {
	MessageIDs map[string]bool `json:"message_ids"`
}

func RunWatcher(root, sourcePath string) (int, error) {
	return RunWatcherWithSource(context.Background(), root, MockFileSource{Path: sourcePath})
}

func RunWatcherWithSource(ctx context.Context, root string, source MessageSource) (int, error) {
	messages, err := source.Fetch(ctx)
	if err != nil {
		return 0, err
	}
	return IngestMessages(ctx, root, messages)
}

// IngestMessages writes the given messages to the binding's inbox, skipping any
// already seen (dedup by platform:channel:message id) and recording new ones in
// the seen-state. It acquires the watcher lock, so it is safe to call from a
// per-cycle poll and from a long-running push ingester concurrently.
func IngestMessages(ctx context.Context, root string, messages []SourceMessage) (int, error) {
	if err := Init(root); err != nil {
		return 0, err
	}
	lock, err := AcquireLock(pathIn(root, "locks", "watcher.lock"))
	if err != nil {
		return 0, err
	}
	defer lock.Release()

	sort.SliceStable(messages, func(i, j int) bool {
		return messages[i].CreatedAt < messages[j].CreatedAt
	})

	statePath := pathIn(root, "state", "seen_message_ids.json")
	state, err := readSeenState(statePath)
	if err != nil {
		return 0, err
	}

	created := 0
	for _, message := range messages {
		key := seenKey(message)
		if state.MessageIDs[key] {
			continue
		}
		hash, err := HashSource(message)
		if err != nil {
			return created, err
		}
		job := InputJob{
			Schema:    1,
			JobID:     buildJobID(message, hash),
			RequestID: buildRequestID(message, hash),
			InputHash: hash,
			Source:    message,
			Attempt:   0,
			CreatedAt: message.CreatedAt,
		}
		if err := AtomicWriteJSON(pathIn(root, "inbox", "pending", job.JobID+".json"), job); err != nil {
			return created, err
		}
		state.MessageIDs[key] = true
		created++
	}
	if err := AtomicWriteJSON(statePath, state); err != nil {
		return created, err
	}
	return created, nil
}

func readSeenState(path string) (seenState, error) {
	state := seenState{MessageIDs: map[string]bool{}}
	if err := ReadJSON(path, &state); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return state, nil
		}
		return state, err
	}
	if state.MessageIDs == nil {
		state.MessageIDs = map[string]bool{}
	}
	return state, nil
}

func seenKey(message SourceMessage) string {
	return message.Platform + ":" + message.ChannelID + ":" + message.MessageID
}

func buildRequestID(message SourceMessage, inputHash string) string {
	sum := sha256.Sum256([]byte(seenKey(message) + ":" + inputHash))
	return hex.EncodeToString(sum[:16])
}

func buildJobID(message SourceMessage, inputHash string) string {
	stamp := sanitize(message.CreatedAt)
	shortHash := inputHash
	if len(shortHash) > 12 {
		shortHash = shortHash[:12]
	}
	return fmt.Sprintf("%s-%s-%s", stamp, sanitize(message.MessageID), shortHash)
}

var nonJobChar = regexp.MustCompile(`[^A-Za-z0-9]+`)

func sanitize(s string) string {
	s = nonJobChar.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	if s == "" {
		return "unknown"
	}
	return s
}
