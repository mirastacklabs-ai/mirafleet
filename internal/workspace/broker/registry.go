package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	maxReplayBody = 1 << 20 // request bodies up to this size are replayed after a 401 challenge
	maxCache      = 256
)

type cachedToken struct {
	token   string
	expires time.Time
}

type tokenCache struct {
	mu sync.Mutex
	m  map[string]cachedToken
}

func (c *tokenCache) get(k string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.m[k]
	if !ok || time.Now().After(t.expires) {
		delete(c.m, k)
		return "", false
	}
	return t.token, true
}

func (c *tokenCache) put(k, tok string, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= maxCache {
		c.m = map[string]cachedToken{}
	}
	c.m[k] = cachedToken{tok, time.Now().Add(ttl)}
}

// repoName extracts the repository from /v2/<name>/(blobs|manifests|tags|referrers)/..., the
// cache key under which a scoped token is reused.
func repoName(path string) string {
	p := strings.TrimPrefix(path, "/v2/")
	for _, kw := range []string{"/blobs/", "/manifests/", "/tags/", "/referrers/"} {
		if i := strings.Index(p, kw); i > 0 {
			return p[:i]
		}
	}
	return ""
}

// parseChallenge parses `Bearer realm="…",service="…",scope="…"`.
func parseChallenge(h string) (map[string]string, bool) {
	h = strings.TrimSpace(h)
	if len(h) < 7 || !strings.EqualFold(h[:7], "bearer ") {
		return nil, false
	}
	h = h[7:]
	out := map[string]string{}
	for {
		h = strings.TrimLeft(h, " ,")
		eq := strings.IndexByte(h, '=')
		if eq <= 0 {
			break
		}
		key := strings.ToLower(strings.TrimSpace(h[:eq]))
		h = h[eq+1:]
		var val string
		if strings.HasPrefix(h, `"`) {
			end := strings.IndexByte(h[1:], '"')
			if end < 0 {
				return nil, false
			}
			val, h = h[1:1+end], h[end+2:]
		} else {
			end := strings.IndexByte(h, ',')
			if end < 0 {
				end = len(h)
			}
			val, h = strings.TrimSpace(h[:end]), h[end:]
		}
		out[key] = val
	}
	return out, out["realm"] != ""
}

// registryDo sends r upstream; on a Bearer 401 challenge it fetches a token with the stored
// credential, injects it and retries, so the client never sees the challenge or the token.
func (b *Broker) registryDo(ctx context.Context, cred Credential, hostport string, r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil && r.Body != http.NoBody {
		if r.ContentLength < 0 || r.ContentLength > maxReplayBody {
			body = nil // streamed; cannot be replayed
		} else {
			var err error
			if body, err = io.ReadAll(r.Body); err != nil {
				return nil, err
			}
			r.Body.Close()
		}
	}
	replayable := r.Body == nil || r.Body == http.NoBody || body != nil
	send := func(tok string) (*http.Response, error) {
		a := r.Clone(ctx)
		if body != nil {
			a.Body = io.NopCloser(bytes.NewReader(body))
		}
		if tok != "" {
			a.Header.Set("Authorization", "Bearer "+tok)
		}
		return b.tr.RoundTrip(a)
	}
	key := cred.Name + "|" + hostport + "|" + repoName(r.URL.Path)
	tok, _ := b.tokens.get(key)
	resp, err := send(tok)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	ch, ok := parseChallenge(resp.Header.Get("Www-Authenticate"))
	if !ok || !replayable {
		return resp, nil
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	tok, ttl, err := b.fetchToken(ctx, cred, ch)
	if err != nil {
		return nil, err
	}
	b.tokens.put(key, tok, ttl)
	return send(tok)
}

// fetchToken calls the challenge realm with the stored credential. The realm must be https and
// one of the credential's own hosts, so the secret cannot be sent elsewhere.
func (b *Broker) fetchToken(ctx context.Context, cred Credential, ch map[string]string) (string, time.Duration, error) {
	u, err := url.Parse(ch["realm"])
	if err != nil || u.Scheme != "https" || u.User != nil {
		return "", 0, errors.New("registry realm not allowed")
	}
	host, port, err := normHost(u.Host)
	if err != nil {
		return "", 0, errors.New("registry realm not allowed")
	}
	if c, ok := b.cfg.Provider.Lookup(host, port); !ok || c.Name != cred.Name {
		return "", 0, errors.New("registry realm not allowed")
	}
	q := u.Query()
	if v := ch["service"]; v != "" {
		q.Set("service", v)
	}
	if v := ch["scope"]; v != "" {
		q.Set("scope", v)
	}
	u.RawQuery = q.Encode()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", 0, err
	}
	req.SetBasicAuth(cred.User, cred.Secret)
	resp, err := b.tr.RoundTrip(req)
	if err != nil {
		return "", 0, errors.New("registry token request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, errors.New("registry token request refused")
	}
	var tr struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tr); err != nil {
		return "", 0, errors.New("registry token response invalid")
	}
	tok := tr.Token
	if tok == "" {
		tok = tr.AccessToken
	}
	if tok == "" {
		return "", 0, errors.New("registry token response empty")
	}
	ttl := 60 * time.Second
	if tr.ExpiresIn > 0 {
		ttl = time.Duration(tr.ExpiresIn) * time.Second
	}
	if ttl -= 10 * time.Second; ttl < time.Second {
		ttl = time.Second
	}
	return tok, ttl, nil
}
