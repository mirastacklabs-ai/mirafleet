package osb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ExecdPort is the in-sandbox port of execd.
const ExecdPort = 44772

const (
	apiKeyHeader   = "OPEN-SANDBOX-API-KEY"
	execdTokHeader = "X-EXECD-ACCESS-TOKEN"
	maxJSONBody    = 8 << 20
	maxErrBody     = 512
	maxListPages   = 1000
	defaultPage    = 100
)

var (
	idRe       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	proxyPathR = regexp.MustCompile(`^(?:/v1)?/sandboxes/([A-Za-z0-9][A-Za-z0-9._-]{0,127})/(?:proxy|port)/` + strconv.Itoa(ExecdPort) + `$`)
)

// secret hides a value from fmt (it is only ever held by pointer, so even %+v of an enclosing
// struct prints an address, not the value).
type secret struct{ v string }

func (*secret) String() string   { return "[redacted]" }
func (*secret) GoString() string { return "[redacted]" }

// APIError is a non-2xx answer from OpenSandbox. Message is truncated and scrubbed of secrets.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("osb: server returned %d", e.StatusCode)
	}
	return fmt.Sprintf("osb: server returned %d: %s", e.StatusCode, e.Message)
}

// IsNotFound reports whether err is an APIError with status 404.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound
}

// Client talks to OpenSandbox through the TLS front. It is safe for concurrent use.
type Client struct {
	hc      *http.Client
	base    *url.URL
	apiKey  *secret
	timeout time.Duration

	mu   sync.Mutex
	seen map[string]*secret // every execd token handed out, scrubbed from all errors
}

// NewClient builds the hardened HTTP client and reads the API key file once.
func NewClient(cfg Config) (*Client, error) {
	hc, err := NewHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	base, _ := parseBase(cfg.BaseURL)
	if cfg.APIKeyFile == "" {
		return nil, errors.New("osb: api_key_file is required")
	}
	key, err := readKey(cfg.APIKeyFile)
	if err != nil {
		return nil, err
	}
	t := cfg.CallTimeout
	if t <= 0 {
		t = defaultCallTimeout
	}
	return &Client{hc: hc, base: base, apiKey: &secret{key}, timeout: t, seen: map[string]*secret{}}, nil
}

func readKey(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", errors.New("osb: cannot read api_key_file")
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o177 != 0 {
		return "", errors.New("osb: api_key_file must be mode 0600 or 0400")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("osb: cannot read api_key_file")
	}
	k := strings.TrimSpace(string(b))
	if k == "" || strings.ContainsAny(k, "\r\n\x00") {
		return "", errors.New("osb: api_key_file is empty or malformed")
	}
	return k, nil
}

// String never reveals the key.
func (c *Client) String() string { return "osb.Client{" + c.base.Host + "}" }

// GoString never reveals the key.
func (c *Client) GoString() string { return c.String() }

// ---- lifecycle types ----

// Image is the sandbox image.
type Image struct {
	URI string `json:"uri"`
}

// EgressRule is one network policy rule.
type EgressRule struct {
	Action string `json:"action"`
	Target string `json:"target"`
}

// NetworkPolicy is the per-sandbox egress policy.
type NetworkPolicy struct {
	DefaultAction string       `json:"defaultAction"`
	Egress        []EgressRule `json:"egress,omitempty"`
}

// PVC describes a named volume claim.
type PVC struct {
	ClaimName                  string `json:"claimName"`
	CreateIfNotExists          bool   `json:"createIfNotExists,omitempty"`
	DeleteOnSandboxTermination bool   `json:"deleteOnSandboxTermination,omitempty"`
}

// Volume is a sandbox volume.
type Volume struct {
	Name      string `json:"name"`
	PVC       *PVC   `json:"pvc,omitempty"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// CreateRequest is the body of POST /v1/sandboxes.
type CreateRequest struct {
	Image          Image             `json:"image"`
	Entrypoint     []string          `json:"entrypoint"`
	Env            map[string]string `json:"env,omitempty"`
	ResourceLimits map[string]string `json:"resourceLimits"`
	Timeout        int               `json:"timeout,omitempty"` // seconds, >= 60
	Metadata       map[string]string `json:"metadata,omitempty"`
	NetworkPolicy  *NetworkPolicy    `json:"networkPolicy,omitempty"`
	Volumes        []Volume          `json:"volumes,omitempty"`
}

// Status is a sandbox status.
type Status struct {
	State   string `json:"state"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// Sandbox is the lifecycle representation of a sandbox.
type Sandbox struct {
	ID        string            `json:"id"`
	Status    Status            `json:"status"`
	Metadata  map[string]string `json:"metadata"`
	ExpiresAt string            `json:"expiresAt"`
	CreatedAt string            `json:"createdAt"`
}

// ListOptions filters List. Metadata is sent to the server (AND semantics) and re-checked
// client-side, so a server that ignores the filter cannot leak foreign sandboxes into results.
type ListOptions struct {
	States   []string
	Metadata map[string]string
	PageSize int
}

// Endpoint is a validated execd route. Its token is never printed.
type Endpoint struct {
	// Path is the proxy path on the configured base, e.g. /sandboxes/<id>/proxy/44772.
	Path  string
	token *secret
}

// String redacts the token.
func (e *Endpoint) String() string { return "osb.Endpoint{" + e.Path + "}" }

// GoString redacts the token.
func (e *Endpoint) GoString() string { return e.String() }

// ---- plumbing ----

func (c *Client) scrub(s string, extra ...*secret) string {
	c.mu.Lock()
	known := make([]*secret, 0, len(c.seen)+1+len(extra))
	known = append(known, c.apiKey)
	for _, t := range c.seen {
		known = append(known, t)
	}
	c.mu.Unlock()
	for _, sec := range append(known, extra...) {
		if sec != nil && sec.v != "" {
			s = strings.ReplaceAll(s, sec.v, "[redacted]")
		}
	}
	return s
}

// scrubErr removes secrets from transport errors (defence in depth; they should not contain any).
func (c *Client) scrubErr(err error, extra ...*secret) error {
	if err == nil {
		return nil
	}
	m := c.scrub(err.Error(), extra...)
	if m == err.Error() {
		return err
	}
	return errors.New(m)
}

func (c *Client) apiError(resp *http.Response, extra ...*secret) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
	msg := strings.TrimSpace(string(b))
	var j struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &j) == nil && j.Message != "" {
		msg = j.Message
	}
	msg = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, msg)
	return &APIError{StatusCode: resp.StatusCode, Message: c.scrub(msg, extra...)}
}

func (c *Client) url(path string, q url.Values) string {
	u := *c.base
	u.Path = path
	u.RawQuery = q.Encode()
	return u.String()
}

// do sends a request; cancel must be called once the response body is finished with.
func (c *Client) do(ctx context.Context, method, rawURL string, hdr http.Header, body io.Reader, ep *Endpoint, stream bool) (*http.Response, func(), error) {
	cancel := func() {}
	if !stream {
		var cc context.CancelFunc
		ctx, cc = context.WithTimeout(ctx, c.timeout)
		cancel = cc
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		cancel()
		return nil, nil, errors.New("osb: build request")
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", "mirafleet-osb")
	req.Header.Set(apiKeyHeader, c.apiKey.v)
	var tok *secret
	if ep != nil && ep.token != nil && ep.token.v != "" {
		req.Header.Set(execdTokHeader, ep.token.v)
		tok = ep.token
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		cancel()
		return nil, nil, c.scrubErr(err, tok)
	}
	return resp, cancel, nil
}

// jsonCall performs a JSON request and decodes a 2xx answer into out (if non-nil).
func (c *Client) jsonCall(ctx context.Context, method, rawURL string, in, out any, ep *Endpoint, okStatus ...int) error {
	var body io.Reader
	hdr := http.Header{"Accept": {"application/json"}}
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return errors.New("osb: encode request")
		}
		body = bytes.NewReader(b)
		hdr.Set("Content-Type", "application/json")
	}
	resp, cancel, err := c.do(ctx, method, rawURL, hdr, body, ep, false)
	if err != nil {
		return err
	}
	defer cancel()
	defer resp.Body.Close()
	var tok *secret
	if ep != nil {
		tok = ep.token
	}
	if !statusOK(resp.StatusCode, okStatus) {
		return c.apiError(resp, tok)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxJSONBody))
		return nil
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBody))
	if err := dec.Decode(out); err != nil {
		return errors.New("osb: decode response")
	}
	return nil
}

func statusOK(got int, ok []int) bool {
	if len(ok) == 0 {
		return got >= 200 && got < 300
	}
	for _, s := range ok {
		if s == got {
			return true
		}
	}
	return false
}

func checkID(id string) error {
	if !idRe.MatchString(id) {
		return errors.New("osb: invalid sandbox id")
	}
	return nil
}

// ---- lifecycle calls ----

// CreateSandbox creates a sandbox (POST /v1/sandboxes, 202 or 201).
func (c *Client) CreateSandbox(ctx context.Context, req CreateRequest) (Sandbox, error) {
	var sb Sandbox
	err := c.jsonCall(ctx, http.MethodPost, c.url("/v1/sandboxes", nil), req, &sb, nil, 200, 201, 202)
	if err == nil {
		err = checkID(sb.ID)
	}
	return sb, err
}

// GetSandbox fetches one sandbox. A missing sandbox yields an error for which IsNotFound is true.
func (c *Client) GetSandbox(ctx context.Context, id string) (Sandbox, error) {
	var sb Sandbox
	if err := checkID(id); err != nil {
		return sb, err
	}
	err := c.jsonCall(ctx, http.MethodGet, c.url("/v1/sandboxes/"+id, nil), nil, &sb, nil)
	return sb, err
}

// DeleteSandbox deletes a sandbox. 404 counts as done (nil).
func (c *Client) DeleteSandbox(ctx context.Context, id string) error {
	if err := checkID(id); err != nil {
		return err
	}
	err := c.jsonCall(ctx, http.MethodDelete, c.url("/v1/sandboxes/"+id, nil), nil, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// ListSandboxes walks every page (bounded) and returns the sandboxes that match opt.Metadata.
// An empty metadata filter is refused: the client never lists sandboxes it cannot attribute.
func (c *Client) ListSandboxes(ctx context.Context, opt ListOptions) ([]Sandbox, error) {
	if len(opt.Metadata) == 0 {
		return nil, errors.New("osb: refusing to list sandboxes without a metadata filter")
	}
	size := opt.PageSize
	if size <= 0 {
		size = defaultPage
	}
	var out []Sandbox
	for page := 1; page <= maxListPages; page++ {
		q := url.Values{}
		q.Set("page", strconv.Itoa(page))
		q.Set("pageSize", strconv.Itoa(size))
		for _, s := range opt.States {
			q.Add("state", s)
		}
		if len(opt.Metadata) > 0 {
			m := url.Values{}
			for k, v := range opt.Metadata {
				m.Set(k, v)
			}
			q.Set("metadata", m.Encode())
		}
		var resp struct {
			Items      []Sandbox `json:"items"`
			Pagination struct {
				HasNextPage bool `json:"hasNextPage"`
			} `json:"pagination"`
		}
		if err := c.jsonCall(ctx, http.MethodGet, c.url("/v1/sandboxes", q), nil, &resp, nil); err != nil {
			return nil, err
		}
		for _, sb := range resp.Items {
			if matchMeta(sb.Metadata, opt.Metadata) {
				out = append(out, sb)
			}
		}
		if !resp.Pagination.HasNextPage {
			return out, nil
		}
	}
	return nil, errors.New("osb: list exceeded page limit")
}

func matchMeta(have, want map[string]string) bool {
	for k, v := range want {
		if got, ok := have[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// GetEndpoint asks for the server-proxied execd route and returns it rebuilt on the configured
// base: only the path of the returned endpoint is used, after strict validation. Any host or
// scheme the server returns is ignored, so a hostile or misconfigured server cannot steer the
// runner to another host or to plain http. Headers other than the execd token are dropped.
func (c *Client) GetEndpoint(ctx context.Context, id string) (*Endpoint, error) {
	if err := checkID(id); err != nil {
		return nil, err
	}
	q := url.Values{"use_server_proxy": {"true"}}
	var r struct {
		Endpoint string            `json:"endpoint"`
		Headers  map[string]string `json:"headers"`
	}
	raw := c.url("/v1/sandboxes/"+id+"/endpoints/"+strconv.Itoa(ExecdPort), q)
	if err := c.jsonCall(ctx, http.MethodGet, raw, nil, &r, nil); err != nil {
		return nil, err
	}
	p, err := proxyPath(r.Endpoint, id)
	if err != nil {
		return nil, err
	}
	ep := &Endpoint{Path: p}
	for k, v := range r.Headers {
		if strings.EqualFold(k, execdTokHeader) && v != "" && !strings.ContainsAny(v, "\r\n\x00") {
			ep.token = &secret{v}
			c.mu.Lock()
			if len(c.seen) > 4096 {
				c.seen = map[string]*secret{}
			}
			c.seen[v] = ep.token
			c.mu.Unlock()
		}
	}
	return ep, nil
}

// proxyPath extracts and validates the path of a server-returned endpoint string. Accepted
// shapes: "https://host/path", "host:port/path" and "/path". The host part is discarded.
func proxyPath(endpoint, id string) (string, error) {
	bad := errors.New("osb: server returned an unacceptable execd endpoint")
	s := endpoint
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
		j := strings.IndexByte(s, '/')
		if j < 0 {
			return "", bad
		}
		s = s[j:]
	} else if !strings.HasPrefix(s, "/") {
		j := strings.IndexByte(s, '/')
		if j < 0 {
			return "", bad
		}
		s = s[j:]
	}
	if strings.ContainsAny(s, "?#%\\") {
		return "", bad
	}
	m := proxyPathR.FindStringSubmatch(s)
	if m == nil || m[1] != id {
		return "", bad
	}
	return s, nil
}

// ---- execd calls ----

func (c *Client) execdURL(ep *Endpoint, suffix string, q url.Values) (string, error) {
	if ep == nil || !proxyPathR.MatchString(ep.Path) {
		return "", errors.New("osb: invalid endpoint")
	}
	return c.url(ep.Path+suffix, q), nil
}

// CommandRequest is the body of POST /command.
type CommandRequest struct {
	Argv    []string          `json:"argv"`
	Cwd     string            `json:"cwd,omitempty"`
	Envs    map[string]string `json:"envs,omitempty"`
	Timeout int64             `json:"timeout,omitempty"` // milliseconds
	UID     *int              `json:"uid,omitempty"`
	GID     *int              `json:"gid,omitempty"`
}

// CommandStream is a running command's event stream. Close it to abort the request.
type CommandStream struct {
	r      *SSEReader
	body   io.Closer
	cancel context.CancelFunc
}

// Next returns the next event (io.EOF at the end).
func (s *CommandStream) Next() (Event, error) { return s.r.Next() }

// Close aborts the request and releases the connection.
func (s *CommandStream) Close() error {
	s.cancel()
	return s.body.Close()
}

// RunCommand starts a command and returns its SSE stream. ctx bounds the whole stream.
func (c *Client) RunCommand(ctx context.Context, ep *Endpoint, req CommandRequest) (*CommandStream, error) {
	raw, err := c.execdURL(ep, "/command", nil)
	if err != nil {
		return nil, err
	}
	if len(req.Argv) == 0 {
		return nil, errors.New("osb: empty argv")
	}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, errors.New("osb: encode request")
	}
	sctx, cancel := context.WithCancel(ctx)
	resp, _, err := c.do(sctx, http.MethodPost, raw, http.Header{
		"Content-Type": {"application/json"}, "Accept": {"text/event-stream"},
	}, bytes.NewReader(b), ep, true)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer cancel()
		defer resp.Body.Close()
		return nil, c.apiError(resp, ep.token)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		cancel()
		resp.Body.Close()
		return nil, errors.New("osb: command answer is not an event stream")
	}
	return &CommandStream{r: NewSSEReader(resp.Body, 0, 0), body: resp.Body, cancel: cancel}, nil
}

// CancelCommand interrupts a running command by its init id.
func (c *Client) CancelCommand(ctx context.Context, ep *Endpoint, id string) error {
	if id == "" || len(id) > 256 || strings.ContainsAny(id, "\r\n\x00") {
		return errors.New("osb: invalid command id")
	}
	raw, err := c.execdURL(ep, "/command", url.Values{"id": {id}})
	if err != nil {
		return err
	}
	return c.jsonCall(ctx, http.MethodDelete, raw, nil, nil, ep)
}

// FileInfo is execd's file metadata.
type FileInfo struct {
	Path       string `json:"path"`
	Type       string `json:"type"`
	Size       int64  `json:"size"`
	ModifiedAt string `json:"modified_at"`
	CreatedAt  string `json:"created_at"`
	Owner      string `json:"owner"`
	Group      string `json:"group"`
	Mode       int    `json:"mode"`
}

func fsPrefix(uid, gid int) (string, error) {
	if uid < 0 || gid < 0 {
		return "", errors.New("osb: invalid uid/gid")
	}
	return "/v1/filesystem/" + strconv.Itoa(uid) + "/" + strconv.Itoa(gid) + "/files", nil
}

// StatFile returns info for path, running as uid/gid. A missing file yields IsNotFound.
func (c *Client) StatFile(ctx context.Context, ep *Endpoint, uid, gid int, path string) (FileInfo, error) {
	var fi FileInfo
	pre, err := fsPrefix(uid, gid)
	if err != nil {
		return fi, err
	}
	raw, err := c.execdURL(ep, pre+"/info", url.Values{"path": {path}})
	if err != nil {
		return fi, err
	}
	var m map[string]FileInfo
	if err := c.jsonCall(ctx, http.MethodGet, raw, nil, &m, ep); err != nil {
		return fi, err
	}
	fi, ok := m[path]
	if !ok {
		if len(m) != 1 {
			return FileInfo{}, errors.New("osb: file info missing from response")
		}
		for _, v := range m {
			fi = v
		}
	}
	return fi, nil
}

// FileBody is a downloaded file range. Close it when done.
type FileBody struct {
	io.ReadCloser
	Status int
	// ContentRange is the server's Content-Range header (set for 206).
	ContentRange string
}

type cancelCloser struct {
	io.Closer
	cancel func()
}

func (c cancelCloser) Close() error { defer c.cancel(); return c.Closer.Close() }

// DownloadFile fetches path. With end >= start it sends "Range: bytes=start-end"; with
// end < 0 and start > 0 it sends an open-ended range; with start == 0 and end < 0, no Range.
func (c *Client) DownloadFile(ctx context.Context, ep *Endpoint, uid, gid int, path string, start, end int64) (*FileBody, error) {
	pre, err := fsPrefix(uid, gid)
	if err != nil {
		return nil, err
	}
	raw, err := c.execdURL(ep, pre+"/download", url.Values{"path": {path}})
	if err != nil {
		return nil, err
	}
	hdr := http.Header{}
	switch {
	case start < 0:
		return nil, errors.New("osb: invalid range")
	case end >= 0 && end < start:
		return nil, errors.New("osb: invalid range")
	case end >= 0:
		hdr.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	case start > 0:
		hdr.Set("Range", fmt.Sprintf("bytes=%d-", start))
	}
	resp, cancel, err := c.do(ctx, http.MethodGet, raw, hdr, nil, ep, false)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		defer cancel()
		defer resp.Body.Close()
		return nil, c.apiError(resp, ep.token)
	}
	return &FileBody{
		ReadCloser: struct {
			io.Reader
			io.Closer
		}{resp.Body, cancelCloser{resp.Body, cancel}},
		Status:       resp.StatusCode,
		ContentRange: resp.Header.Get("Content-Range"),
	}, nil
}

// UploadMeta is the metadata part of an upload. Owner and Group are names or numeric ids.
type UploadMeta struct {
	Path  string `json:"path"`
	Owner string `json:"owner,omitempty"`
	Group string `json:"group,omitempty"`
	Mode  int    `json:"mode"` // octal digits written as decimal, e.g. 644, as execd expects
}

// UploadFile uploads one file via multipart: a metadata part, then the file part.
func (c *Client) UploadFile(ctx context.Context, ep *Endpoint, uid, gid int, meta UploadMeta, data io.Reader) error {
	pre, err := fsPrefix(uid, gid)
	if err != nil {
		return err
	}
	raw, err := c.execdURL(ep, pre+"/upload", nil)
	if err != nil {
		return err
	}
	mb, err := json.Marshal(meta)
	if err != nil {
		return errors.New("osb: encode metadata")
	}
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		werr := func() error {
			h := textproto.MIMEHeader{}
			h.Set("Content-Disposition", `form-data; name="metadata"; filename="metadata"`)
			h.Set("Content-Type", "application/json")
			p, err := mw.CreatePart(h)
			if err != nil {
				return err
			}
			if _, err = p.Write(mb); err != nil {
				return err
			}
			h = textproto.MIMEHeader{}
			h.Set("Content-Disposition", `form-data; name="file"; filename="file"`)
			h.Set("Content-Type", "application/octet-stream")
			if p, err = mw.CreatePart(h); err != nil {
				return err
			}
			if _, err = io.Copy(p, data); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(werr)
	}()
	resp, cancel, err := c.do(ctx, http.MethodPost, raw, http.Header{"Content-Type": {mw.FormDataContentType()}}, pr, ep, false)
	if err != nil {
		pr.CloseWithError(err)
		return err
	}
	defer cancel()
	defer resp.Body.Close()
	pr.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return c.apiError(resp, ep.token)
	}
	return nil
}
