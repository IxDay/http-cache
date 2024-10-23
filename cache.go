/*
MIT License

Copyright (c) 2018 Victor Springer

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package cache

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Response is the cached response data structure.
type Response struct {
	// Value is the cached response value.
	Value []byte

	// Header is the cached response header.
	Header http.Header

	// Expiration is the cached response expiration date.
	Expiration time.Time

	// LastAccess is the last date a cached response was accessed.
	// Used by LRU and MRU algorithms.
	LastAccess time.Time

	// Frequency is the count of times a cached response is accessed.
	// Used for LFU and MFU algorithms.
	Frequency int
}

// Client data structure for HTTP cache middleware.
type Client struct {
	adapter            Adapter
	ttl                time.Duration
	refreshKey         string
	methods            []string
	writeExpiresHeader bool
	vary               []string
	generateKey        GenerateKey
}

// ClientOption is used to set Client settings.
type ClientOption func(c *Client) error

// Adapter interface for HTTP cache middleware client.
type Adapter interface {
	// Get retrieves the cached response by a given key. It also
	// returns true or false, whether it exists or not.
	Get(key uint64) ([]byte, bool)

	// Set caches a response for a given key until an expiration date.
	Set(key uint64, response []byte, expiration time.Time)

	// Release frees cache for a given key.
	Release(key uint64)
}

type GenerateKey func(*http.Request) []byte

// Middleware is the HTTP cache middleware handler.
func (c *Client) Middleware(next http.Handler) http.Handler {
	vary := strings.Join(c.vary, ",")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !c.cacheableMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}

		sortURLParams(r.URL)
		key := c.hash(r)

		params := r.URL.Query()
		if _, ok := params[c.refreshKey]; ok {
			delete(params, c.refreshKey)

			r.URL.RawQuery = params.Encode()
			key = c.hash(r)

			c.adapter.Release(key)
		} else {
			b, ok := c.adapter.Get(key)
			response := BytesToResponse(b)
			if ok {
				if response.Expiration.After(time.Now()) {
					response.LastAccess = time.Now()
					response.Frequency++
					c.adapter.Set(key, response.Bytes(), response.Expiration)

					//w.WriteHeader(http.StatusNotModified)
					for k, v := range response.Header {
						w.Header().Set(k, strings.Join(v, ","))
					}
					if c.writeExpiresHeader {
						w.Header().Set("Expires", response.Expiration.UTC().Format(http.TimeFormat))
					}
					if vary != "" {
						w.Header().Set("Vary", vary)
					}
					w.Write(response.Value)
					return
				}

				c.adapter.Release(key)
			}
		}

		rw := &responseWriter{ResponseWriter: w}
		next.ServeHTTP(rw, r)

		statusCode := rw.statusCode
		value := rw.body
		now := time.Now()
		expires := now.Add(c.ttl)
		if statusCode < 400 {
			response := Response{
				Value:      value,
				Header:     rw.Header(),
				Expiration: expires,
				LastAccess: now,
				Frequency:  1,
			}
			c.adapter.Set(key, response.Bytes(), response.Expiration)
		}
	})
}

func (c *Client) cacheableMethod(method string) bool {
	for _, m := range c.methods {
		if method == m {
			return true
		}
	}
	return false
}

// BytesToResponse converts bytes array into Response data structure.
func BytesToResponse(b []byte) Response {
	var r Response
	dec := gob.NewDecoder(bytes.NewReader(b))
	dec.Decode(&r)

	return r
}

// Bytes converts Response data structure into bytes array.
func (r Response) Bytes() []byte {
	var b bytes.Buffer
	enc := gob.NewEncoder(&b)
	enc.Encode(&r)

	return b.Bytes()
}

func sortURLParams(URL *url.URL) {
	params := URL.Query()
	for _, param := range params {
		sort.Slice(param, func(i, j int) bool {
			return param[i] < param[j]
		})
	}
	URL.RawQuery = params.Encode()
}

// KeyAsString can be used by adapters to convert the cache key from uint64 to string.
func KeyAsString(key uint64) string {
	return strconv.FormatUint(key, 36)
}

func (c *Client) hash(r *http.Request) uint64 {
	hash := fnv.New64a()
	hash.Write(c.generateKey(r))
	for _, header := range c.vary {
		if value := strings.Join(r.Header.Values(header), ""); value != "" {
			hash.Write([]byte(value))
		}
	}
	return hash.Sum64()
}

func DefaultGenerateKey(r *http.Request) []byte {
	if r.Method == http.MethodPost && r.Body != nil {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil
		}
		r.Body = io.NopCloser(bytes.NewBuffer(body))
		return append([]byte(r.URL.String()), body...)
	}
	return []byte(r.URL.String())
}

// NewClient initializes the cache HTTP middleware client with the given
// options.
func NewClient(opts ...ClientOption) (*Client, error) {
	c := &Client{}

	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}

	if c.adapter == nil {
		return nil, errors.New("cache client adapter is not set")
	}
	if int64(c.ttl) < 1 {
		return nil, errors.New("cache client ttl is not set")
	}
	if c.methods == nil {
		c.methods = []string{http.MethodGet}
	}
	if c.generateKey == nil {
		c.generateKey = DefaultGenerateKey
	}

	return c, nil
}

// ClientWithAdapter sets the adapter type for the HTTP cache
// middleware client.
func ClientWithAdapter(a Adapter) ClientOption {
	return func(c *Client) error {
		c.adapter = a
		return nil
	}
}

// ClientWithTTL sets how long each response is going to be cached.
func ClientWithTTL(ttl time.Duration) ClientOption {
	return func(c *Client) error {
		if int64(ttl) < 1 {
			return fmt.Errorf("cache client ttl %v is invalid", ttl)
		}

		c.ttl = ttl

		return nil
	}
}

// ClientWithRefreshKey sets the parameter key used to free a request
// cached response. Optional setting.
func ClientWithRefreshKey(refreshKey string) ClientOption {
	return func(c *Client) error {
		c.refreshKey = refreshKey
		return nil
	}
}

// ClientWithMethods sets the acceptable HTTP methods to be cached.
// Optional setting. If not set, default is "GET".
func ClientWithMethods(methods []string) ClientOption {
	return func(c *Client) error {
		for _, method := range methods {
			if method != http.MethodGet && method != http.MethodPost {
				return fmt.Errorf("invalid method %s", method)
			}
		}
		c.methods = methods
		return nil
	}
}

// ClientWithHeaders
func ClientWithGenerateKey(fn GenerateKey) ClientOption {
	return func(c *Client) error {
		c.generateKey = fn
		return nil
	}
}

func ClientWithVary(headers ...string) ClientOption {
	return func(c *Client) error {
		c.vary = headers
		return nil
	}
}

// ClientWithExpiresHeader enables middleware to add an Expires header to responses.
// Optional setting. If not set, default is false.
func ClientWithExpiresHeader() ClientOption {
	return func(c *Client) error {
		c.writeExpiresHeader = true
		return nil
	}
}

type responseWriter struct {
	http.ResponseWriter
	statusCode int
	body       []byte
}

func (w *responseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *responseWriter) Write(b []byte) (int, error) {
	w.body = b
	return w.ResponseWriter.Write(b)
}
