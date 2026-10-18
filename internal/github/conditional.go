package github

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
)

type cachedRepresentation struct {
	etag   string
	header http.Header
	body   []byte
}
type conditionalTransport struct {
	base http.RoundTripper
	*conditionalCache
}
type conditionalCache struct {
	mu      sync.Mutex
	entries map[string]cachedRepresentation
	size    int
}

// DiscoveryReader has a bounded, in-memory GET cache scoped to this client's
// immutable credentials. Every reuse requires a fresh authenticated 304.
// Publication clients and their preflight/readback methods are unchanged.
func (c *Client) DiscoveryReader() *Client {
	c.ReuseDiscoveryCache(nil)
	return &Client{host: c.host, endpoint: c.endpoint, token: c.token, http: c.discoveryHTTP}
}

// ReuseDiscoveryCache retains only validated GET representations across watcher
// reconnects. Fresh credentials and the new client's transport are still used;
// host, endpoint or credential changes cannot inherit another account's cache.
func (c *Client) ReuseDiscoveryCache(previous *Client) {
	c.discoveryOnce.Do(func() {
		cache := &conditionalCache{entries: map[string]cachedRepresentation{}}
		if previous != nil && c.host == previous.host && c.endpoint.String() == previous.endpoint.String() && c.token == previous.token {
			previous.ReuseDiscoveryCache(nil)
			cache = previous.discoveryCache
		}
		copy := *c.http
		transport := copy.Transport
		if transport == nil {
			transport = http.DefaultTransport
		}
		copy.Transport = &conditionalTransport{base: transport, conditionalCache: cache}
		c.discoveryCache = cache
		c.discoveryHTTP = &copy
	})
}
func (t *conditionalTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method != http.MethodGet {
		return t.base.RoundTrip(request)
	}
	key := request.URL.String()
	t.mu.Lock()
	cached, exists := t.entries[key]
	t.mu.Unlock()
	sent := request.Clone(request.Context())
	if exists && sent.Header.Get("If-None-Match") == "" {
		sent.Header.Set("If-None-Match", cached.etag)
	}
	response, err := t.base.RoundTrip(sent)
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusNotModified {
		response.Body.Close()
		if !exists || sent.Header.Get("If-None-Match") != cached.etag {
			return nil, errors.New("conditional response has no matching cached representation")
		}
		headers := cached.header.Clone()
		for key, values := range response.Header {
			headers[key] = append([]string{}, values...)
		}
		if strings.Contains(strings.ToLower(headers.Get("Cache-Control")), "no-store") {
			t.remove(key)
		}
		response.StatusCode = http.StatusOK
		response.Status = "200 OK"
		response.Header = headers
		response.Body = io.NopCloser(bytes.NewReader(cached.body))
		response.ContentLength = int64(len(cached.body))
		return response, nil
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("ETag") == "" || strings.Contains(strings.ToLower(response.Header.Get("Cache-Control")), "no-store") {
		if strings.Contains(strings.ToLower(response.Header.Get("Cache-Control")), "no-store") {
			t.remove(key)
		}
		return response, nil
	}
	// The ordinary client decoder has the same 8 MiB completeness bound.
	original := response.Body
	data, readErr := io.ReadAll(io.LimitReader(original, 8*1024*1024+1))
	if readErr != nil {
		original.Close()
		return nil, readErr
	}
	if len(data) > 8*1024*1024 {
		response.Body = &replayBody{Reader: io.MultiReader(bytes.NewReader(data), original), closer: original}
		return response, nil
	}
	original.Close()
	response.Body = io.NopCloser(bytes.NewReader(data))
	entry := cachedRepresentation{etag: response.Header.Get("ETag"), header: response.Header.Clone(), body: data}
	t.mu.Lock()
	if old, ok := t.entries[key]; ok {
		t.size -= len(old.body)
		delete(t.entries, key)
	}
	if t.size+len(data) > 32*1024*1024 {
		t.entries = map[string]cachedRepresentation{}
		t.size = 0
	}
	t.entries[key] = entry
	t.size += len(data)
	t.mu.Unlock()
	return response, nil
}
func (t *conditionalTransport) remove(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if old, ok := t.entries[key]; ok {
		t.size -= len(old.body)
		delete(t.entries, key)
	}
}

type replayBody struct {
	io.Reader
	closer io.Closer
}

func (r *replayBody) Close() error { return r.closer.Close() }
