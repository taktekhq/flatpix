// Package s3test is a tiny in-memory S3 server for tests: path-style GET,
// HEAD and PUT of single objects, enough for the S3 origin and the S3 cache.
package s3test

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Object is a stored object.
type Object struct {
	Data        []byte
	ContentType string
	ETag        string
}

// Server is the fake. Keys are "bucket/key".
type Server struct {
	*httptest.Server
	mu      sync.Mutex
	objects map[string]*Object
	Gets    atomic.Int64
	Heads   atomic.Int64
	Puts    atomic.Int64
}

// New starts a server. Close it when done.
func New() *Server {
	s := &Server{objects: map[string]*Object{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// Put stores an object directly.
func (s *Server) Put(bucketKey string, data []byte, contentType string) {
	sum := md5.Sum(data)
	s.mu.Lock()
	s.objects[bucketKey] = &Object{Data: data, ContentType: contentType, ETag: hex.EncodeToString(sum[:])}
	s.mu.Unlock()
}

// Get reads an object directly.
func (s *Server) Get(bucketKey string) (*Object, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[bucketKey]
	return o, ok
}

// Len is the number of objects.
func (s *Server) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.objects)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/")
	switch r.Method {
	case http.MethodHead, http.MethodGet:
		if r.Method == http.MethodHead {
			s.Heads.Add(1)
		} else {
			s.Gets.Add(1)
		}
		o, ok := s.Get(key)
		if !ok {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			if r.Method == http.MethodGet {
				fmt.Fprint(w, `<?xml version="1.0"?><Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>`)
			}
			return
		}
		w.Header().Set("ETag", `"`+o.ETag+`"`)
		w.Header().Set("Last-Modified", time.Unix(1700000000, 0).UTC().Format(http.TimeFormat))
		w.Header().Set("Content-Type", o.ContentType)
		w.Header().Set("Accept-Ranges", "bytes")
		data := o.Data
		status := http.StatusOK
		if rng := r.Header.Get("Range"); rng != "" && r.Method == http.MethodGet {
			var a, b int
			rest := strings.TrimPrefix(rng, "bytes=")
			lo, hi, _ := strings.Cut(rest, "-")
			a, _ = strconv.Atoi(lo)
			b = len(data) - 1
			if hi != "" {
				b, _ = strconv.Atoi(hi)
			}
			if b >= len(data) {
				b = len(data) - 1
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
			data = data[a : b+1]
			status = http.StatusPartialContent
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(status)
		if r.Method == http.MethodGet {
			w.Write(data)
		}
	case http.MethodPut:
		s.Puts.Add(1)
		body := r.Body
		var data []byte
		if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
			data = decodeChunked(body)
		} else {
			data, _ = io.ReadAll(body)
		}
		s.Put(key, data, r.Header.Get("Content-Type"))
		o, _ := s.Get(key)
		w.Header().Set("ETag", `"`+o.ETag+`"`)
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "unsupported", http.StatusMethodNotAllowed)
	}
}

// decodeChunked reads an aws-chunked body: "<hex size>;chunk-signature=...\r\n<data>\r\n".
func decodeChunked(r io.Reader) []byte {
	br := bufio.NewReader(r)
	var out bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return out.Bytes()
		}
		sizeHex, _, _ := strings.Cut(strings.TrimSpace(line), ";")
		n, err := strconv.ParseInt(sizeHex, 16, 64)
		if err != nil || n == 0 {
			return out.Bytes()
		}
		io.CopyN(&out, br, n)
		br.Discard(2)
	}
}
