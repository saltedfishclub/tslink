package core

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// LogAttr is one flattened structured field. Groups are folded into the key
// with dots so the GUI can render a single flat line per entry.
type LogAttr struct {
	Key   string
	Value string
}

// LogEntry is a single captured log record.
type LogEntry struct {
	Seq   uint64
	Time  time.Time
	Level slog.Level
	Msg   string
	Attrs []LogAttr
	// Source is the value of the conventional "from" attribute, used by the
	// GUI to group logs by subsystem.
	Source string
}

// Text renders the entry the way the console handler would, minus colour.
func (e LogEntry) Text() string {
	var b strings.Builder
	b.WriteString(e.Time.Format("2006-01-02 15:04:05.000"))
	b.WriteByte(' ')
	b.WriteString(levelLabel(e.Level))
	b.WriteByte(' ')
	b.WriteString(e.Msg)
	for _, a := range e.Attrs {
		b.WriteByte(' ')
		b.WriteString(a.Key)
		b.WriteByte('=')
		if strings.ContainsAny(a.Value, " \t\"") {
			fmt.Fprintf(&b, "%q", a.Value)
		} else {
			b.WriteString(a.Value)
		}
	}
	return b.String()
}

func levelLabel(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return "DBG"
	case l < slog.LevelWarn:
		return "INF"
	case l < slog.LevelError:
		return "WRN"
	default:
		return "ERR"
	}
}

// LevelLabel exposes the three-letter level name used in exports and the GUI.
func LevelLabel(l slog.Level) string { return levelLabel(l) }

// LogQuery filters a buffer snapshot.
type LogQuery struct {
	// MinLevel drops anything below it.
	MinLevel slog.Level
	// Text is a case-insensitive substring matched against the message, the
	// attribute values and the source.
	Text string
	// Source, when set, keeps only entries from that subsystem.
	Source string
	// Limit keeps only the newest N matches. Zero means unlimited.
	Limit int
}

func (q LogQuery) match(e LogEntry) bool {
	if e.Level < q.MinLevel {
		return false
	}
	if q.Source != "" && e.Source != q.Source {
		return false
	}
	if q.Text == "" {
		return true
	}
	needle := strings.ToLower(q.Text)
	if strings.Contains(strings.ToLower(e.Msg), needle) {
		return true
	}
	if strings.Contains(strings.ToLower(e.Source), needle) {
		return true
	}
	for _, a := range e.Attrs {
		if strings.Contains(strings.ToLower(a.Key), needle) ||
			strings.Contains(strings.ToLower(a.Value), needle) {
			return true
		}
	}
	return false
}

// LogBuffer is a fixed-capacity ring of the most recent log records. It is the
// single source of truth for the GUI's log view and for diagnostic exports.
//
// All methods are safe for concurrent use.
type LogBuffer struct {
	mu       sync.RWMutex
	entries  []LogEntry // ring storage, len == cap once full
	start    int        // index of the oldest entry
	count    int
	nextSeq  uint64
	dropped  uint64
	subs     map[int]chan struct{}
	nextSub  int
	sources  map[string]int
	levelCnt map[slog.Level]int
}

// DefaultLogCapacity is how many records the GUI keeps in memory. At roughly
// 200 bytes per record this is a few megabytes at most.
const DefaultLogCapacity = 20000

// NewLogBuffer returns a buffer holding at most capacity records.
func NewLogBuffer(capacity int) *LogBuffer {
	if capacity <= 0 {
		capacity = DefaultLogCapacity
	}
	return &LogBuffer{
		entries:  make([]LogEntry, capacity),
		subs:     make(map[int]chan struct{}),
		sources:  make(map[string]int),
		levelCnt: make(map[slog.Level]int),
	}
}

// Add appends an entry, evicting the oldest record when full.
func (b *LogBuffer) Add(e LogEntry) {
	b.mu.Lock()
	b.nextSeq++
	e.Seq = b.nextSeq

	capacity := len(b.entries)
	if b.count == capacity {
		evicted := b.entries[b.start]
		b.decStatsLocked(evicted)
		b.entries[b.start] = e
		b.start = (b.start + 1) % capacity
		b.dropped++
	} else {
		b.entries[(b.start+b.count)%capacity] = e
		b.count++
	}
	b.incStatsLocked(e)

	for _, ch := range b.subs {
		select {
		case ch <- struct{}{}:
		default: // subscriber has a pending wakeup already
		}
	}
	b.mu.Unlock()
}

func (b *LogBuffer) incStatsLocked(e LogEntry) {
	b.levelCnt[e.Level]++
	if e.Source != "" {
		b.sources[e.Source]++
	}
}

func (b *LogBuffer) decStatsLocked(e LogEntry) {
	b.levelCnt[e.Level]--
	if b.levelCnt[e.Level] <= 0 {
		delete(b.levelCnt, e.Level)
	}
	if e.Source != "" {
		b.sources[e.Source]--
		if b.sources[e.Source] <= 0 {
			delete(b.sources, e.Source)
		}
	}
}

// Len returns the number of buffered records.
func (b *LogBuffer) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.count
}

// Dropped returns how many records were evicted because the ring was full.
func (b *LogBuffer) Dropped() uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.dropped
}

// LastSeq returns the sequence number of the most recent record.
func (b *LogBuffer) LastSeq() uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.nextSeq
}

// Counts returns how many buffered records exist per level.
func (b *LogBuffer) Counts() map[slog.Level]int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make(map[slog.Level]int, len(b.levelCnt))
	for k, v := range b.levelCnt {
		out[k] = v
	}
	return out
}

// Sources returns the distinct subsystem names currently buffered, sorted.
func (b *LogBuffer) Sources() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, 0, len(b.sources))
	for k := range b.sources {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Snapshot returns every buffered record, oldest first.
func (b *LogBuffer) Snapshot() []LogEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.collectLocked(func(LogEntry) bool { return true }, 0)
}

// Tail returns the newest n records, oldest first.
//
// It walks backwards from the newest record so the cost is O(n), not O(ring).
// The GUI's log overlay calls this on every frame; scanning a full 20k-entry
// ring each time was enough on its own to keep a core busy.
func (b *LogBuffer) Tail(n int) []LogEntry {
	if n <= 0 {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.newestLocked(func(LogEntry) bool { return true }, n)
}

// Filter returns the records matching q, oldest first.
func (b *LogBuffer) Filter(q LogQuery) []LogEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if q.Limit > 0 {
		return b.newestLocked(q.match, q.Limit)
	}
	return b.collectLocked(q.match, 0)
}

// newestLocked walks the ring newest-first, keeping at most limit matches, and
// returns them oldest-first.
func (b *LogBuffer) newestLocked(keep func(LogEntry) bool, limit int) []LogEntry {
	capacity := len(b.entries)
	out := make([]LogEntry, 0, min(limit, b.count))
	for i := b.count - 1; i >= 0 && len(out) < limit; i-- {
		e := b.entries[(b.start+i)%capacity]
		if keep(e) {
			out = append(out, e)
		}
	}
	// Reverse in place to restore chronological order.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// collectLocked walks the ring oldest-first. When limit > 0 only the newest
// limit matches are kept.
func (b *LogBuffer) collectLocked(keep func(LogEntry) bool, limit int) []LogEntry {
	capacity := len(b.entries)
	out := make([]LogEntry, 0, min(b.count, 512))
	for i := 0; i < b.count; i++ {
		e := b.entries[(b.start+i)%capacity]
		if keep(e) {
			out = append(out, e)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// Subscribe returns a channel that receives a value whenever a record is
// added, plus a function that cancels the subscription. The channel is
// buffered and coalescing: a slow reader sees one wakeup, not a backlog.
func (b *LogBuffer) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	id := b.nextSub
	b.nextSub++
	b.subs[id] = ch
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, id)
			b.mu.Unlock()
		})
	}
	return ch, cancel
}

// ---------------------------------------------------------------------------
// slog handler
// ---------------------------------------------------------------------------

// bufHandler tees records into a LogBuffer and on to a wrapped handler.
type bufHandler struct {
	buf    *LogBuffer
	next   slog.Handler
	attrs  []LogAttr
	groups []string
}

// Handler returns a slog.Handler that records everything into b and forwards
// to next. next may be nil, in which case records are only buffered.
//
// The buffer always captures at debug level regardless of what next filters,
// so the GUI can show detail the console suppressed.
func (b *LogBuffer) Handler(next slog.Handler) slog.Handler {
	return &bufHandler{buf: b, next: next}
}

func (h *bufHandler) Enabled(ctx context.Context, l slog.Level) bool {
	// Always capture: the buffer is the diagnostic record of last resort.
	return true
}

func (h *bufHandler) Handle(ctx context.Context, r slog.Record) error {
	attrs := make([]LogAttr, 0, len(h.attrs)+r.NumAttrs())
	attrs = append(attrs, h.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		attrs = appendAttr(attrs, h.groups, a)
		return true
	})

	source := ""
	for _, a := range attrs {
		if a.Key == "from" {
			source = a.Value
		}
	}

	t := r.Time
	if t.IsZero() {
		t = time.Now()
	}
	h.buf.Add(LogEntry{
		Time:   t,
		Level:  r.Level,
		Msg:    r.Message,
		Attrs:  attrs,
		Source: source,
	})

	if h.next != nil && h.next.Enabled(ctx, r.Level) {
		return h.next.Handle(ctx, r)
	}
	return nil
}

func (h *bufHandler) WithAttrs(as []slog.Attr) slog.Handler {
	if len(as) == 0 {
		return h
	}
	clone := *h
	clone.attrs = make([]LogAttr, len(h.attrs), len(h.attrs)+len(as))
	copy(clone.attrs, h.attrs)
	for _, a := range as {
		clone.attrs = appendAttr(clone.attrs, h.groups, a)
	}
	if h.next != nil {
		clone.next = h.next.WithAttrs(as)
	}
	return &clone
}

func (h *bufHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := *h
	clone.groups = append(append([]string(nil), h.groups...), name)
	if h.next != nil {
		clone.next = h.next.WithGroup(name)
	}
	return &clone
}

// appendAttr flattens a slog.Attr, expanding groups into dotted keys.
func appendAttr(dst []LogAttr, groups []string, a slog.Attr) []LogAttr {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return dst
	}
	if a.Value.Kind() == slog.KindGroup {
		sub := a.Value.Group()
		if len(sub) == 0 {
			return dst
		}
		nested := groups
		if a.Key != "" {
			nested = append(append([]string(nil), groups...), a.Key)
		}
		for _, s := range sub {
			dst = appendAttr(dst, nested, s)
		}
		return dst
	}
	key := a.Key
	if len(groups) > 0 {
		key = strings.Join(groups, ".") + "." + key
	}
	return append(dst, LogAttr{Key: key, Value: a.Value.String()})
}

// NewLoggerWithBuffer builds the console logger exactly as [NewLogger] does
// and tees every record into buf.
func NewLoggerWithBuffer(level string, useJsonFormat bool, buf *LogBuffer) *slog.Logger {
	base := NewLogger(level, useJsonFormat)
	logger := slog.New(buf.Handler(base.Handler()))
	slog.SetDefault(logger)
	return logger
}

// ---------------------------------------------------------------------------
// Export
// ---------------------------------------------------------------------------

// secretPattern matches Tailscale auth keys and OAuth client secrets, which
// are the one thing in these logs that must never reach a paste service.
var secretPattern = regexp.MustCompile(`\b(tskey-[a-zA-Z]+-)[A-Za-z0-9\-_]{6,}`)

// secretKeys are attribute names whose values are replaced wholesale.
var secretKeys = map[string]bool{
	"auth_key":      true,
	"authkey":       true,
	"auth-key":      true,
	"token":         true,
	"secret":        true,
	"password":      true,
	"client_secret": true,
}

// Redact removes credentials from a single string.
func Redact(s string) string {
	return secretPattern.ReplaceAllString(s, "${1}REDACTED")
}

func redactAttr(a LogAttr) LogAttr {
	if secretKeys[strings.ToLower(a.Key)] {
		if a.Value == "" {
			return a
		}
		return LogAttr{Key: a.Key, Value: "[REDACTED]"}
	}
	a.Value = Redact(a.Value)
	return a
}

// ExportOptions controls how a log dump is rendered.
type ExportOptions struct {
	Query LogQuery
	// Redact strips credentials. Callers sharing logs publicly must leave this
	// on; it defaults to on because [ExportText] is built for sharing.
	NoRedact bool
	// Header is prepended verbatim, used for environment metadata.
	Header string
}

// ExportText renders matching entries as a plain-text report suitable for
// pasting into an issue tracker or a paste service.
func (b *LogBuffer) ExportText(opt ExportOptions) string {
	entries := b.Filter(opt.Query)

	var sb strings.Builder
	if opt.Header != "" {
		sb.WriteString(opt.Header)
		if !strings.HasSuffix(opt.Header, "\n") {
			sb.WriteByte('\n')
		}
		sb.WriteString("\n")
	}
	if dropped := b.Dropped(); dropped > 0 {
		fmt.Fprintf(&sb, "# %d earlier record(s) were dropped from the ring buffer\n\n", dropped)
	}
	for _, e := range entries {
		if !opt.NoRedact {
			e.Msg = Redact(e.Msg)
			redacted := make([]LogAttr, len(e.Attrs))
			for i, a := range e.Attrs {
				redacted[i] = redactAttr(a)
			}
			e.Attrs = redacted
		}
		sb.WriteString(e.Text())
		sb.WriteByte('\n')
	}
	if len(entries) == 0 {
		sb.WriteString("(no matching log entries)\n")
	}
	return sb.String()
}
