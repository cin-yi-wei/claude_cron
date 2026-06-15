package channelagent

import "context"

type MessageSource interface {
	Fetch(ctx context.Context) ([]SourceMessage, error)
}

type MockFileSource struct {
	Path string
}

func (s MockFileSource) Fetch(_ context.Context) ([]SourceMessage, error) {
	var messages []SourceMessage
	if err := ReadJSON(s.Path, &messages); err != nil {
		return nil, err
	}
	return messages, nil
}
