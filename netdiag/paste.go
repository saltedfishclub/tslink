package netdiag

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// pasteUserAgent identifies tslink to the paste services. 0x0.st rejects the
// Go default user agent with 403, so this is not merely cosmetic.
const pasteUserAgent = "tslink/1.0 (+diagnostics)"

// pasteTimeout bounds a single upload attempt, including connect, write and
// the read of the response body.
const pasteTimeout = 20 * time.Second

// MaxPasteBytes is the largest payload accepted by [Upload]. Bigger dumps are
// rejected rather than truncated: the tail of a log is usually the part the
// helper needs, and silently dropping it wastes everyone's time.
const MaxPasteBytes = 1 << 20

// pasteReadLimit caps how much of a response body is read back. A URL is a
// couple hundred bytes; anything larger is an error page.
const pasteReadLimit = 64 << 10

// PasteTarget is one supported paste service.
type PasteTarget struct {
	Key  string // stable id used by the UI
	Name string // human label
	Note string // short caveat: retention, region reachability
}

// pasteService is a target plus the code that performs the upload.
type pasteService struct {
	PasteTarget
	upload func(ctx context.Context, text string) (string, error)
}

// pasteServices is the internal registry, in preference order.
var pasteServices = []pasteService{
	{
		PasteTarget: PasteTarget{
			Key:  "0x0",
			Name: "0x0.st",
			Note: "保留 30 天以上（按大小递减），境内访问可能较慢",
		},
		upload: uploadNullPointer,
	},
	{
		PasteTarget: PasteTarget{
			Key:  "paste_rs",
			Name: "paste.rs",
			Note: "无固定保留期，容量满后自动淘汰旧内容",
		},
		upload: uploadPasteRS,
	},
	{
		PasteTarget: PasteTarget{
			Key:  "dpaste",
			Name: "dpaste.org",
			Note: "保留 7 天后自动删除",
		},
		upload: uploadDpaste,
	},
	{
		PasteTarget: PasteTarget{
			Key:  "termbin",
			Name: "termbin.com",
			Note: "纯 TCP (9999)，HTTPS 被墙时仍可用；保留约 1 个月",
		},
		upload: uploadTermbin,
	},
}

// PasteTargets lists the supported services in preference order.
func PasteTargets() []PasteTarget {
	out := make([]PasteTarget, 0, len(pasteServices))
	for _, s := range pasteServices {
		out = append(out, s.PasteTarget)
	}
	return out
}

// PasteResult is a successful upload.
type PasteResult struct {
	URL      string
	Target   string
	Bytes    int
	Uploaded time.Time
}

// ErrPasteEmpty is returned when there is nothing to upload.
var ErrPasteEmpty = errors.New("netdiag: refusing to upload empty text")

// ErrPasteTooLarge is returned when the payload exceeds [MaxPasteBytes].
var ErrPasteTooLarge = fmt.Errorf("netdiag: text exceeds the %d byte paste limit", MaxPasteBytes)

// Upload sends text to the named target and returns the resulting public URL.
// An empty targetKey tries every target in [PasteTargets] order and returns the
// first success; when all of them fail the returned error names each failure.
//
// The caller MUST redact the text before calling: uploading is an outbound
// publication of user data to a third party. core.LogBuffer.ExportText performs
// that redaction (leave ExportOptions.NoRedact false). The resulting paste is
// PUBLIC — anyone holding the URL can read it, and most of these services offer
// no way to delete it afterwards.
//
// Every attempt is bounded by a ~20s timeout and honours ctx.
func Upload(ctx context.Context, targetKey, text string, logger *slog.Logger) (*PasteResult, error) {
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With(slog.String("from", "paste"))

	if strings.TrimSpace(text) == "" {
		return nil, ErrPasteEmpty
	}
	if len(text) > MaxPasteBytes {
		return nil, fmt.Errorf("%w (got %d bytes); filter the log before sharing", ErrPasteTooLarge, len(text))
	}

	candidates := pasteServices
	if targetKey != "" {
		svc, ok := lookupPasteService(targetKey)
		if !ok {
			return nil, fmt.Errorf("netdiag: unknown paste target %q", targetKey)
		}
		candidates = []pasteService{svc}
	}

	var failures []string
	for _, svc := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		res, err := attemptPaste(ctx, svc, text)
		if err != nil {
			logger.With(
				slog.String("target", svc.Key),
				slog.String("error", err.Error()),
			).Debug("paste upload failed")
			failures = append(failures, fmt.Sprintf("%s: %v", svc.Key, err))
			continue
		}
		logger.With(
			slog.String("target", res.Target),
			slog.String("url", res.URL),
			slog.Int("bytes", res.Bytes),
		).Info("uploaded diagnostic paste")
		return res, nil
	}

	if len(candidates) == 1 {
		return nil, fmt.Errorf("netdiag: upload to %s failed: %s", candidates[0].Key, strings.TrimPrefix(failures[0], candidates[0].Key+": "))
	}
	return nil, fmt.Errorf("netdiag: every paste target failed: %s", strings.Join(failures, "; "))
}

// attemptPaste runs one upload under its own timeout and validates the URL the
// service handed back.
func attemptPaste(ctx context.Context, svc pasteService, text string) (*PasteResult, error) {
	ctx, cancel := context.WithTimeout(ctx, pasteTimeout)
	defer cancel()

	raw, err := svc.upload(ctx, text)
	if err != nil {
		return nil, err
	}
	clean, err := normalisePasteURL(raw)
	if err != nil {
		return nil, err
	}
	return &PasteResult{
		URL:      clean,
		Target:   svc.Key,
		Bytes:    len(text),
		Uploaded: time.Now(),
	}, nil
}

// lookupPasteService finds a service by its stable key.
func lookupPasteService(key string) (pasteService, bool) {
	for _, s := range pasteServices {
		if s.Key == key {
			return s, true
		}
	}
	return pasteService{}, false
}

// ---------------------------------------------------------------------------
// Response validation
// ---------------------------------------------------------------------------

// normalisePasteURL trims a service response down to a single http(s) URL.
// termbin answers with a bare host such as "termbin.com/abcd", so a missing
// scheme is tolerated and upgraded to https. Anything that smells like an HTML
// error page is rejected outright.
func normalisePasteURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	// termbin pads its reply with NULs and terminal escapes.
	s = strings.Trim(s, "\x00\r\n\t ")
	if s == "" {
		return "", errors.New("empty response")
	}
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if looksLikeHTML(s) {
		return "", fmt.Errorf("service returned an error page: %s", snippet(s))
	}
	if len(s) > 512 {
		return "", fmt.Errorf("response is not a URL: %s", snippet(s))
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}

	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("response is not a URL: %s", snippet(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("response has unexpected scheme %q", u.Scheme)
	}
	if u.Host == "" || !strings.Contains(u.Host, ".") {
		return "", fmt.Errorf("response has no usable host: %s", snippet(s))
	}
	return u.String(), nil
}

// looksLikeHTML reports whether s is the beginning of an HTML document rather
// than a URL.
func looksLikeHTML(s string) bool {
	lower := strings.ToLower(strings.TrimSpace(s))
	return strings.HasPrefix(lower, "<") ||
		strings.Contains(lower, "<html") ||
		strings.Contains(lower, "<!doctype")
}

// snippet shortens an untrusted response for inclusion in an error message.
func snippet(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

// ---------------------------------------------------------------------------
// HTTP plumbing
// ---------------------------------------------------------------------------

// pasteHTTPClient is shared by the HTTP-based targets. The per-attempt context
// timeout is the real deadline; the client timeout is a backstop.
var pasteHTTPClient = &http.Client{
	Timeout: pasteTimeout,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return nil
	},
}

// doPaste issues one request and returns the (size-limited) response body.
func doPaste(ctx context.Context, method, endpoint, contentType string, body []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", pasteUserAgent)
	req.Header.Set("Accept", "text/plain, */*")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.ContentLength = int64(len(body))

	resp, err := pasteHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, pasteReadLimit))
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("http %d: %s", resp.StatusCode, snippet(string(data)))
	}
	return string(data), nil
}

// uploadNullPointer posts to 0x0.st as multipart/form-data.
func uploadNullPointer(ctx context.Context, text string) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", "tslink-log.txt")
	if err != nil {
		return "", err
	}
	if _, err := io.WriteString(part, text); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	return doPaste(ctx, http.MethodPost, "https://0x0.st/", mw.FormDataContentType(), buf.Bytes())
}

// uploadPasteRS posts the raw text to paste.rs.
func uploadPasteRS(ctx context.Context, text string) (string, error) {
	return doPaste(ctx, http.MethodPost, "https://paste.rs/", "text/plain; charset=utf-8", []byte(text))
}

// uploadDpaste posts a urlencoded form to dpaste.org and asks for a bare URL
// back rather than the JSON representation.
func uploadDpaste(ctx context.Context, text string) (string, error) {
	form := url.Values{
		"content": {text},
		"lexer":   {"text"},
		"format":  {"url"},
		"expires": {"604800"},
	}
	return doPaste(ctx, http.MethodPost, "https://dpaste.org/api/",
		"application/x-www-form-urlencoded", []byte(form.Encode()))
}

// ---------------------------------------------------------------------------
// termbin (raw TCP)
// ---------------------------------------------------------------------------

// termbinAddr is the netcat-style endpoint termbin.com exposes.
const termbinAddr = "termbin.com:9999"

// uploadTermbin writes the text over a plain TCP connection, half-closes the
// write side so the server knows the paste is complete, then reads the URL it
// replies with. No TLS is involved, which is exactly why this target survives
// environments where the HTTPS paste sites are unreachable.
func uploadTermbin(ctx context.Context, text string) (string, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", termbinAddr)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	// Bound the whole exchange, and make sure a hung read is interrupted when
	// the caller cancels: a blocked socket must never freeze the GUI.
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(pasteTimeout))
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return "", errors.New("termbin: connection is not tcp")
	}
	if _, err := io.WriteString(tcp, text); err != nil {
		return "", fmt.Errorf("termbin: write: %w", err)
	}
	// Half-close: termbin only answers once it sees EOF on its read side.
	if err := tcp.CloseWrite(); err != nil {
		return "", fmt.Errorf("termbin: close write: %w", err)
	}

	data, err := io.ReadAll(io.LimitReader(tcp, pasteReadLimit))
	if err != nil {
		return "", fmt.Errorf("termbin: read: %w", err)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}
	return string(data), nil
}
