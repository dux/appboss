package logstore

import (
	"time"

	_ "modernc.org/sqlite"
)

// Cap on rows kept for retry when the database is temporarily unwritable. Beyond it the oldest
// rows are dropped so a permanently broken database cannot grow the heap without bound.
const (
	maxBufferedRequests = 8192
	maxBufferedLogs     = 32768
)

// RequestEntry is one proxied request. Process is the service that answered it; Country is the
// visitor country the edge reported, empty when there is none.
type RequestEntry struct {
	Time       time.Time `json:"time"`
	Method     string    `json:"method"`
	Host       string    `json:"host"`
	Path       string    `json:"path"`
	Status     int       `json:"status"`
	DurationMS int64     `json:"duration_ms"`
	BytesOut   int64     `json:"bytes_out"`
	IP         string    `json:"ip"`
	UserAgent  string    `json:"user_agent"`
	RequestID  string    `json:"request_id"`
	Process    string    `json:"process"`
	Country    string    `json:"country"`
}

// LogEntry is one line of an app's process output.
type LogEntry struct {
	Time      time.Time `json:"time"`
	Source    string    `json:"source"`
	Process   string    `json:"process"`
	Stream    string    `json:"stream"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
	RequestID string    `json:"request_id"`
	Raw       string    `json:"raw"`
}

type Rates struct {
	LastMinute int64
	LastHour   int64
	LastDay    int64
}

// latencySampleLimit bounds how many rows a latency query reads, newest first.
const latencySampleLimit = 50000

// HostApp is the reserved app name that backs dboss's own daemon log. It never collides with a
// discovered app because app process names must match [a-z][a-z0-9_-]*.
const HostApp = "_dboss"

// Channel is one selectable log type in the console: the request table, process stdout, the
// dboss daemon log, or one app log file.
type Channel struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// AppTree is one app in the log viewer's left nav: the sqlite files on disk and the channels
// inside them.
type AppTree struct {
	Name     string    `json:"name"`
	Bytes    int64     `json:"bytes"`
	Channels []Channel `json:"channels"`
}

// TailOffset is how far the file tailer has read into one app log file.
type TailOffset struct {
	Path   string
	Inode  uint64
	Offset int64
}

// LogFilter narrows SearchLogs. Channel selects one log type: "stdout", "dboss" or "file:<path>".
// Query is full-text over message and raw.
type LogFilter struct {
	Channel string
	Process string
	Level   string
	Query   string
	Since   time.Time
	Before  time.Time
	Limit   int
}

// RequestFilter narrows SearchRequests. Query matches host, path, ip or user agent. Status is an
// exact code; StatusClass (200, 300, 400 or 500) matches a whole class. Process selects the
// service that answered.
type RequestFilter struct {
	Method      string
	Process     string
	Status      int
	StatusClass int
	Query       string
	Since       time.Time
	Before      time.Time
	Limit       int
}

// AuditEntry is one operator action: who did what to which app, and how it turned out. Audit rows
// live in the reserved host database.
type AuditEntry struct {
	ID     int64     `json:"id"`
	Time   time.Time `json:"time"`
	Actor  string    `json:"actor"`
	App    string    `json:"app"`
	Action string    `json:"action"`
	Detail string    `json:"detail"`
	Result string    `json:"result"`
	Error  string    `json:"error,omitempty"`
}

// AuditFilter narrows SearchAudit. ID addresses one row and ignores the other fields.
type AuditFilter struct {
	ID     int64
	App    string
	Actor  string
	Action string
	Since  time.Time
	Before time.Time
	Limit  int
}
