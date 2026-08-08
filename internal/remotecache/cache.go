// Package remotecache downloads external email images through a privacy-safe
// HTTP client and stores them content-addressed for offline rendering.
package remotecache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jsnjack/mailbox/internal/httpclient"
)

const maxImageBytes = 10 << 20 // 10 MiB per external image

// Entry is one cached image resource.
type Entry struct {
	Key  string
	Path string
	MIME string
}

// Cache owns the on-disk image cache and coalesces concurrent fetches of the
// same URL. The production client rejects private/link-local destinations so a
// crafted email cannot turn image loading into an SSRF primitive.
type Cache struct {
	dir    string
	client *http.Client
	locks  sync.Map // sha256 key -> *sync.Mutex
}

// New creates a cache using the hardened production HTTP client.
func New(dir string) *Cache { return &Cache{dir: dir, client: safeClient()} }

// NewWithClient is intended for deterministic tests. Callers outside tests
// should use New so private-network destinations remain blocked.
func NewWithClient(dir string, client *http.Client) *Cache {
	return &Cache{dir: dir, client: client}
}

// Get returns a cached URL, downloading it only when allowNetwork is true.
// Non-image responses, oversized payloads, and unsafe URLs are rejected.
func (c *Cache) Get(ctx context.Context, rawURL string, allowNetwork bool) (Entry, bool, error) {
	normalized, err := normalizeURL(rawURL)
	if err != nil {
		return Entry{}, false, err
	}
	key := urlKey(normalized)
	if entry, ok := c.Open(key); ok {
		return entry, true, nil
	}
	if !allowNetwork {
		return Entry{}, false, nil
	}
	muValue, _ := c.locks.LoadOrStore(key, &sync.Mutex{})
	mu := muValue.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	if entry, ok := c.Open(key); ok {
		return entry, true, nil
	}
	entry, err := c.fetch(ctx, normalized, key)
	if err != nil {
		return Entry{}, false, err
	}
	return entry, true, nil
}

// Key returns the cache key a URL will resolve to, without touching the network
// or the disk. The key is derived from the normalized URL, so a document can
// name a cached image before the cache holds it and let the fetch happen when
// the image is actually requested. An unusable URL is an error, and the caller
// should drop the reference rather than name it.
func Key(rawURL string) (string, error) {
	normalized, err := normalizeURL(rawURL)
	if err != nil {
		return "", err
	}
	return urlKey(normalized), nil
}

// Open resolves a content key without network access.
func (c *Cache) Open(key string) (Entry, bool) {
	if len(key) != sha256.Size*2 {
		return Entry{}, false
	}
	if _, err := hex.DecodeString(key); err != nil {
		return Entry{}, false
	}
	dir := filepath.Join(c.dir, key[:2])
	path := filepath.Join(dir, key+".data")
	mimePath := filepath.Join(dir, key+".mime")
	mimeBytes, err := os.ReadFile(mimePath)
	if err != nil {
		return Entry{}, false
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() <= 0 || fi.Size() > maxImageBytes {
		return Entry{}, false
	}
	mimeType := strings.TrimSpace(string(mimeBytes))
	if !allowedImageMIME(mimeType) {
		return Entry{}, false
	}
	return Entry{Key: key, Path: path, MIME: mimeType}, true
}

func (c *Cache) fetch(ctx context.Context, rawURL, key string) (Entry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Entry{}, err
	}
	req.Header.Set("Accept", "image/avif,image/webp,image/*;q=0.9")
	req.Header.Set("User-Agent", "Mailbox/remote-image-cache")
	resp, err := c.client.Do(req)
	if err != nil {
		return Entry{}, fmt.Errorf("download external image: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Entry{}, fmt.Errorf("download external image: HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxImageBytes {
		return Entry{}, fmt.Errorf("external image exceeds %d bytes", maxImageBytes)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return Entry{}, fmt.Errorf("read external image: %w", err)
	}
	if len(data) == 0 || len(data) > maxImageBytes {
		return Entry{}, fmt.Errorf("external image is empty or too large")
	}
	mimeType, err := safeImageMIME(resp.Header.Get("Content-Type"), data)
	if err != nil {
		return Entry{}, err
	}
	dir := filepath.Join(c.dir, key[:2])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Entry{}, fmt.Errorf("create external image cache: %w", err)
	}
	dataPath := filepath.Join(dir, key+".data")
	mimePath := filepath.Join(dir, key+".mime")
	if err := atomicWrite(dataPath, data); err != nil {
		return Entry{}, err
	}
	if err := atomicWrite(mimePath, []byte(mimeType)); err != nil {
		_ = os.Remove(dataPath)
		return Entry{}, err
	}
	return Entry{Key: key, Path: dataPath, MIME: mimeType}, nil
}

func atomicWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".mailbox-image-*")
	if err != nil {
		return fmt.Errorf("create cache file: %w", err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write cache file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close cache file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("publish cache file: %w", err)
	}
	return nil
}

func normalizeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("invalid external image URL")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported external image scheme %q", u.Scheme)
	}
	if u.User != nil {
		return "", fmt.Errorf("external image URL contains credentials")
	}
	u.Fragment = ""
	return u.String(), nil
}

func urlKey(rawURL string) string {
	sum := sha256.Sum256([]byte(rawURL))
	return hex.EncodeToString(sum[:])
}

func safeImageMIME(declared string, data []byte) (string, error) {
	detected := strings.ToLower(strings.TrimSpace(strings.Split(http.DetectContentType(data), ";")[0]))
	declared = strings.ToLower(strings.TrimSpace(strings.Split(declared, ";")[0]))
	if allowedImageMIME(detected) {
		return detected, nil
	}
	// Some newer formats are reported as application/octet-stream by the Go
	// sniffer. Accept a narrow declared image type only for that inconclusive
	// result; never trust it over a detected HTML/text response.
	if detected == "application/octet-stream" && allowedImageMIME(declared) {
		return declared, nil
	}
	return "", fmt.Errorf("external resource is not a supported image (%s)", detected)
}

func allowedImageMIME(mimeType string) bool {
	switch strings.ToLower(mimeType) {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/avif",
		"image/bmp", "image/x-icon", "image/vnd.microsoft.icon":
		return true
	default:
		return false // SVG is deliberately excluded: it is active document content.
	}
}

func safeClient() *http.Client {
	transport := httpclient.PublicTransport(5 * time.Second)
	transport.MaxIdleConns = 20
	transport.MaxIdleConnsPerHost = 2
	transport.IdleConnTimeout = 30 * time.Second
	transport.TLSHandshakeTimeout = 5 * time.Second
	transport.ResponseHeaderTimeout = 8 * time.Second
	return &http.Client{
		Transport: transport,
		Timeout:   12 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many external image redirects")
			}
			_, err := normalizeURL(req.URL.String())
			return err
		},
	}
}
