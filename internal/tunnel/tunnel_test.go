package tunnel

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

// duplex joins two ends the way exec stdio does: two one-way OS pipes,
// buffered by the kernel (an io.Pipe is not, and both SSH ends write their
// version line before reading).
type duplex struct {
	r *os.File
	w *os.File
}

func (d duplex) Read(p []byte) (int, error)  { return d.r.Read(p) }
func (d duplex) Write(p []byte) (int, error) { return d.w.Write(p) }
func (d duplex) Close() error {
	d.w.Close()
	return d.r.Close()
}

func pair() (a, b io.ReadWriteCloser) {
	ar, bw, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	br, aw, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	return duplex{ar, aw}, duplex{br, bw}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func start(t *testing.T, allow Allow) (*Tunnel, chan error) {
	t.Helper()
	inside, host := pair()
	served := make(chan error, 1)
	go func() { served <- Serve(inside, allow, t.Logf) }()
	tun, err := Open(host)
	if err != nil {
		t.Fatal(err)
	}
	return tun, served
}

func anyLoopback(host string, port int) bool { return host == "127.0.0.1" }

// echo answers every connection on l with what it read, uppercased by
// prefix, so a test can tell the far end did the work.
func echo(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			b, _ := io.ReadAll(c)
			c.Write(append([]byte("got:"), b...))
		}()
	}
}

func roundTrip(t *testing.T, port int, msg []byte) []byte {
	t.Helper()
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write(msg)
	c.(*net.TCPConn).CloseWrite()
	b, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestConnectionsInsideReachTheHost(t *testing.T) {
	tun, _ := start(t, anyLoopback)
	defer tun.Close()
	port := freePort(t)
	l, err := tun.Listen(port)
	if err != nil {
		t.Fatal(err)
	}
	go echo(l)

	// Half-close matters: the client finishes sending before it reads, the
	// way git sends a request body and then reads the pack.
	if got := roundTrip(t, port, []byte("hello")); string(got) != "got:hello" {
		t.Fatalf("round trip = %q", got)
	}

	// Many at once, each with its own bytes: a multiplexer that mixed
	// streams would fail here.
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			msg := make([]byte, 256<<10)
			rand.Read(msg)
			msg = append([]byte(fmt.Sprintf("%02d", i)), msg...)
			if got := roundTrip(t, port, msg); !bytes.Equal(got, append([]byte("got:"), msg...)) {
				t.Errorf("stream %d came back different (%d bytes)", i, len(got))
			}
		}()
	}
	wg.Wait()
}

func TestTheServerOpensOnlyWhatItAllows(t *testing.T) {
	allowed := freePort(t)
	tun, _ := start(t, LoopbackPorts(allowed, allowed))
	defer tun.Close()
	if _, err := tun.Listen(allowed); err != nil {
		t.Fatalf("allowed port: %v", err)
	}
	if _, err := tun.Listen(freePort(t)); err == nil {
		t.Fatal("a port outside the range was opened")
	}
	if _, err := tun.client.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", allowed)); err == nil {
		t.Fatal("a non-loopback address was opened")
	}
}

func TestClosingTheTunnelClosesItsListeners(t *testing.T) {
	tun, served := start(t, anyLoopback)
	port := freePort(t)
	if _, err := tun.Listen(port); err != nil {
		t.Fatal(err)
	}
	tun.Close()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("the server outlived its client")
	}
	// The port is free again: the next tunnel can take it.
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("port still held after the tunnel closed: %v", err)
	}
	l.Close()
	<-tun.Done()
	if tun.Err() == nil {
		t.Fatal("Err is nil after Done")
	}
}

func TestTheIsolationCannotOpenChannelsToTheHost(t *testing.T) {
	inside, host := pair()
	go Serve(inside, anyLoopback, t.Logf)
	tun, err := Open(host)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	// The client side would be the one asking in practice, but the server
	// must refuse anything other than forwards whoever asks.
	if _, _, err := tun.client.OpenChannel("session", nil); err == nil {
		t.Fatal("a session channel was accepted")
	}
	if _, err := tun.client.Dial("tcp", "127.0.0.1:22"); err == nil {
		t.Fatal("a direct-tcpip channel was accepted")
	}
}

func TestAPortItsHolderIsReleasingIsTakenOnceFree(t *testing.T) {
	port := freePort(t)
	// A tunnel being replaced still holds the port for a moment.
	held, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(400*time.Millisecond, func() { held.Close() })
	tun, _ := start(t, anyLoopback)
	l, err := tun.Listen(port)
	if err != nil {
		t.Fatalf("the port was not taken once free: %v", err)
	}
	defer l.Close()
	go echo(l)
	if got := roundTrip(t, port, []byte("hi")); string(got) != "got:hi" {
		t.Fatalf("%q", got)
	}
}

func TestAPortHeldForGoodIsRefusedNotWaitedOnForever(t *testing.T) {
	defer func(d time.Duration) { settleFor = d }(settleFor)
	settleFor = 300 * time.Millisecond
	port := freePort(t)
	held, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	tun, _ := start(t, anyLoopback)
	if _, err := tun.Listen(port); err == nil {
		t.Fatal("a held port was granted")
	}
}
