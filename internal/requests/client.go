package requests

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const maxBody = 8 << 20

var errBackend = errors.New("bookwarehouse response unavailable or invalid")

// client talks to the Bookwarehouse API under <base_url>/api/v1. It never
// follows redirects, so the key is sent only to the configured address.
type client struct {
	base      url.URL
	key       string
	http      *http.Client
	transport *http.Transport
}

func validBase(raw string, allowHTTP bool) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 4096 || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") || (u.Scheme != "https" && (!allowHTTP || u.Scheme != "http")) || strings.HasSuffix(u.Host, ":") {
		return nil, false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, false
		}
	}
	escaped := strings.ToLower(u.EscapedPath())
	if strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%5c") || strings.Contains(u.Path, "\\") {
		return nil, false
	}
	for _, p := range strings.Split(u.Path, "/") {
		if p == "." || p == ".." {
			return nil, false
		}
	}
	for _, r := range u.Host + u.Path {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return nil, false
		}
	}
	return u, true
}

func newClient(base, key string, allowHTTP bool, timeout time.Duration) (*client, error) {
	u, ok := validBase(base, allowHTTP)
	if !ok || len(key) == 0 || len(key) > 8192 {
		return nil, errBackend
	}
	for _, r := range key {
		if r < 0x21 || r > 0x7e {
			return nil, errBackend
		}
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1"
	u.RawPath = ""
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext, ForceAttemptHTTP2: true, MaxIdleConns: 16, MaxIdleConnsPerHost: 8, IdleConnTimeout: 30 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: timeout, MaxResponseHeaderBytes: 1 << 20, DisableCompression: true}
	return &client{base: *u, key: key, transport: transport, http: &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *client) close() { c.transport.CloseIdleConnections() }

// json sends one request. Code 0 means no response arrived. Any status of
// 300 or more other than 409 is an error, and the body is decoded only on
// success or 409. No caller receives transport errors or backend error text:
// either may contain credentials.
func (c *client) json(ctx context.Context, method, path string, query url.Values, body any, out any) (int, error) {
	u := c.base
	u.Path += path
	u.RawQuery = query.Encode()
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return 0, errBackend
		}
	}
	r, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(data))
	if err != nil {
		return 0, errBackend
	}
	r.Header.Set("X-API-Key", c.key)
	r.Header.Set("Accept", "application/json")
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(r)
	if err != nil {
		return 0, errBackend
	}
	defer func() { _ = response.Body.Close() }()
	data, err = io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil || len(data) > maxBody {
		return response.StatusCode, errBackend
	}
	if response.StatusCode >= 300 && response.StatusCode != 409 {
		return response.StatusCode, errBackend
	}
	if out != nil && json.Unmarshal(data, out) != nil {
		return response.StatusCode, errBackend
	}
	return response.StatusCode, nil
}

// fingerprint identifies the Bookwarehouse a receipt was issued against
// without storing its address: 16 hex digits of a SHA-256 of the base URL.
func fingerprint(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	n := strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + strings.TrimRight(u.EscapedPath(), "/")
	sum := sha256.Sum256([]byte("bookwarehouse-requests:v1:" + n))
	return hex.EncodeToString(sum[:8])
}

func validID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}
