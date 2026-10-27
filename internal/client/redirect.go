package client

import (
	"fmt"
	"net"
	"net/http"
	"time"
)

// CatchRedirect listens on a loopback port for the one redirect a browser
// login ends with. A browser on this Mac lands there by itself; the query it
// brings arrives on the channel as "?…", which is what FinishLogin takes. A
// browser anywhere else cannot reach it, and the operator pastes the address
// instead. stop closes the listener.
func CatchRedirect() (redirect string, got <-chan string, stop func(), err error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, nil, err
	}
	ch := make(chan string, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<!doctype html><meta charset=utf-8><title>gero</title><p>gero received the login; gero says whether it worked. This tab can close.</p>")
		select {
		case ch <- "?" + r.URL.RawQuery:
		default:
		}
	})}
	go srv.Serve(l)
	return fmt.Sprintf("http://127.0.0.1:%d/callback", l.Addr().(*net.TCPAddr).Port), ch, func() { srv.Close() }, nil
}
