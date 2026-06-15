package channelagent

type Attachment struct {
	ID   string `json:"id,omitempty"`
	URL  string `json:"url,omitempty"`
	Type string `json:"type,omitempty"`
}

type SourceMessage struct {
	Platform    string       `json:"platform"`
	ChannelID   string       `json:"channel_id"`
	MessageID   string       `json:"message_id"`
	AuthorID    string       `json:"author_id"`
	CreatedAt   string       `json:"created_at"`
	Content     string       `json:"content"`
	Attachments []Attachment `json:"attachments"`
}

type InputJob struct {
	Schema    int           `json:"schema"`
	JobID     string        `json:"job_id"`
	RequestID string        `json:"request_id"`
	InputHash string        `json:"input_hash"`
	Source    SourceMessage `json:"source"`
	Attempt   int           `json:"attempt"`
	CreatedAt string        `json:"created_at"`
}

type OutputJob struct {
	Schema    int    `json:"schema"`
	JobID     string `json:"job_id"`
	RequestID string `json:"request_id"`
	InputHash string `json:"input_hash"`
	Send      bool   `json:"send"`
	Text      string `json:"text"`
}
