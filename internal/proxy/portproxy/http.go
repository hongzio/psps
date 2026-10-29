package portproxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/hongzio/gero/internal/proxy"
)

// Handler is an http forward: what a browser on the Mac asks goes to the
// isolation's port through dial, without the cookies named in withheld,
// and what comes back cannot set them. A Denied from dial is what the
// browser is told; WebSocket upgrades pass through.
//
// Only a request for a loopback name is taken. A web page elsewhere that
// points a name of its own at 127.0.0.1 (DNS rebinding) would otherwise
// read what the isolation serves, through the Mac's browser.
func Handler(dial func(context.Context) (net.Conn, error), withheld []string) http.Handler {
	held := map[string]bool{}
	for _, n := range withheld {
		held[n] = true
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			// One address for the pool; dial knows where it goes.
			r.Out.URL.Scheme, r.Out.URL.Host = "http", "isolation"
			r.Out.Host = r.In.Host
			withholdCookies(r.Out.Header, held)
		},
		Transport: &http.Transport{
			DialContext:     func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
			IdleConnTimeout: 90 * time.Second,
		},
		ModifyResponse: func(resp *http.Response) error {
			withholdSetCookies(resp.Header, held)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return
			}
			msg := "the isolation's server did not answer: " + err.Error()
			var d *proxy.Denied
			if errors.As(err, &d) {
				msg = d.Reason
			}
			http.Error(w, "gero: "+msg, http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackName(r.Host) {
			http.Error(w, "gero: a forwarded port answers to 127.0.0.1 or localhost, not "+r.Host, http.StatusMisdirectedRequest)
			return
		}
		rp.ServeHTTP(w, r)
	})
}

// loopbackName is a Host a browser only ever sends for the Mac itself:
// browsers resolve localhost and its subdomains themselves, never by DNS.
func loopbackName(hostport string) bool {
	h := hostport
	if hh, _, err := net.SplitHostPort(hostport); err == nil {
		h = hh
	}
	h = strings.TrimSuffix(strings.ToLower(strings.Trim(h, "[]")), ".")
	return h == "127.0.0.1" || h == "::1" || h == "localhost" || strings.HasSuffix(h, ".localhost")
}

// withholdCookies drops the held cookies from a request, keeping the
// others as they were sent.
func withholdCookies(h http.Header, held map[string]bool) {
	lines := h.Values("Cookie")
	if len(lines) == 0 {
		return
	}
	var kept []string
	for _, l := range lines {
		for _, c := range strings.Split(l, ";") {
			c = strings.TrimSpace(c)
			name, _, _ := strings.Cut(c, "=")
			if c != "" && !held[strings.TrimSpace(name)] {
				kept = append(kept, c)
			}
		}
	}
	h.Del("Cookie")
	if len(kept) > 0 {
		h.Set("Cookie", strings.Join(kept, "; "))
	}
}

// withholdSetCookies drops an answer's attempts to set a held cookie: a
// server in the isolation that replaced gero's session would sign the
// browser out of gero.
func withholdSetCookies(h http.Header, held map[string]bool) {
	lines := h.Values("Set-Cookie")
	if len(lines) == 0 {
		return
	}
	h.Del("Set-Cookie")
	for _, l := range lines {
		name, _, _ := strings.Cut(l, "=")
		if !held[strings.TrimSpace(name)] {
			h.Add("Set-Cookie", l)
		}
	}
}
