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
	// Components 是選填的 Discord 訊息元件（例如按鈕的 action row），以原始
	// JSON 直接夾帶。留空時行為與舊版完全相同（只送純文字）。權限閘門用它掛
	// 允許/拒絕按鈕，讓核准以 custom_id 綁定 id、零競態。
	Components []any `json:"components,omitempty"`
	// Attachments 是選填的本機檔案絕對路徑清單，worker 跟 sender 同機同檔案系統，
	// 直接給路徑即可。有值時 Discord 端會改用 multipart 上傳附檔；其他平台目前
	// 忽略這個欄位（純文字照送，不會壞掉）。
	Attachments []string `json:"attachments,omitempty"`
}
