package proxy

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

// HopByHop headers belong to one connection and are never forwarded.
var HopByHop = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true, "proxy-authorization": true,
	"te": true, "trailer": true, "transfer-encoding": true, "upgrade": true,
}

// CopyHeaders copies src into dst minus hop-by-hop headers and the names in
// drop (lower case). Connection's own list of hop-by-hop names is honoured.
func CopyHeaders(dst, src http.Header, drop ...string) {
	skip := map[string]bool{}
	for _, d := range drop {
		skip[strings.ToLower(d)] = true
	}
	for _, v := range src.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			skip[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
	for k, vs := range src {
		low := strings.ToLower(k)
		if HopByHop[low] || skip[low] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// RawTarget is the request target exactly as the client sent it, split
// into path and query. Every decision is made on these bytes, and what goes
// upstream is rebuilt from what the decision approved: a decision made on a
// decoded or cleaned path while the raw one is forwarded is how proxies get
// bypassed.
func RawTarget(r *http.Request) (path, query string) {
	t := r.RequestURI
	for _, scheme := range []string{"http://", "https://"} {
		if strings.HasPrefix(strings.ToLower(t), scheme) {
			rest := t[len(scheme):]
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				t = rest[i:]
			} else {
				t = "/"
			}
			break
		}
	}
	path, query, _ = strings.Cut(t, "?")
	return path, query
}

// Refuse answers with a plain-text reason.
func Refuse(w http.ResponseWriter, status int, reason string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", fmt.Sprint(len(reason)+1))
	w.WriteHeader(status)
	fmt.Fprintln(w, reason)
}

// TokenBucket refuses rather than queues: a service's budget is shared with
// people, dashboards and CI, so a looping agent must exhaust this budget,
// never the org's.
type TokenBucket struct {
	rate, burst float64
	now         func() time.Time

	mu     sync.Mutex
	tokens float64
	stamp  time.Time
}

func NewTokenBucket(perMinute, burst float64, now func() time.Time) *TokenBucket {
	if now == nil {
		now = time.Now
	}
	return &TokenBucket{rate: perMinute / 60, burst: burst, now: now, tokens: burst, stamp: now()}
}

// Allow says whether a request may go now, and if not how long until one
// may.
func (b *TokenBucket) Allow() (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cur := b.now()
	b.tokens = math.Min(b.burst, b.tokens+cur.Sub(b.stamp).Seconds()*b.rate)
	b.stamp = cur
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / b.rate * float64(time.Second))
}

// Buckets keeps one TokenBucket per key for an Instance's life, so a
// budget survives configuration edits and reconnects.
type Buckets struct {
	PerMinute, Burst float64
	mu               sync.Mutex
	m                map[string]*TokenBucket
}

func (bs *Buckets) Get(key string) *TokenBucket {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if bs.m == nil {
		bs.m = map[string]*TokenBucket{}
	}
	b, ok := bs.m[key]
	if !ok {
		b = NewTokenBucket(bs.PerMinute, bs.Burst, nil)
		bs.m[key] = b
	}
	return b
}

// Relay streams a response through, minus hop-by-hop headers and drop. A
// clone body or a query result is routinely hundreds of megabytes, so
// nothing is buffered, and each read is flushed so progress arrives as it is
// sent.
func Relay(w http.ResponseWriter, resp *http.Response, drop ...string) {
	CopyHeaders(w.Header(), resp.Header, append([]string{"content-length"}, drop...)...)
	if resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", fmt.Sprint(resp.ContentLength))
	}
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	buf := make([]byte, 64<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			rc.Flush()
		}
		if err != nil {
			if err != io.EOF {
				// Framing tells the client the body was cut short.
				panic(http.ErrAbortHandler)
			}
			return
		}
	}
}
