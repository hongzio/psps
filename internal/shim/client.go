package shim

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/hongzio/gero/internal/shimproto"
)

// Dialer opens one connection to a shim. In production that is a
// `container exec -i <ctr> gero-shim client` process's stdio; in tests it is
// the shim's unix socket.
type Dialer func(ctx context.Context) (io.ReadWriteCloser, error)

func UnixDialer(path string) Dialer {
	return func(ctx context.Context) (io.ReadWriteCloser, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}
}

type Client struct {
	Dial Dialer
}

// closeOnDone makes a blocking read on conn honour ctx.
func closeOnDone(ctx context.Context, conn io.Closer) (stop func()) {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

func writeJSONLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

func readResponse(r *bufio.Reader) (shimproto.Response, error) {
	var resp shimproto.Response
	line, err := r.ReadBytes('\n')
	if err != nil {
		return resp, fmt.Errorf("shim: read response: %w", err)
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return resp, fmt.Errorf("shim: bad response %q: %w", line, err)
	}
	if !resp.OK {
		return resp, errors.New("shim: " + resp.Error)
	}
	return resp, nil
}

func (c *Client) roundTrip(ctx context.Context, req shimproto.Request) (shimproto.Response, error) {
	conn, err := c.Dial(ctx)
	if err != nil {
		return shimproto.Response{}, err
	}
	defer conn.Close()
	defer closeOnDone(ctx, conn)()
	if err := writeJSONLine(conn, req); err != nil {
		return shimproto.Response{}, err
	}
	return readResponse(bufio.NewReader(conn))
}

func (c *Client) Ping(ctx context.Context) (string, error) {
	resp, err := c.roundTrip(ctx, shimproto.Request{Op: shimproto.OpPing})
	return resp.Version, err
}

func (c *Client) List(ctx context.Context) ([]shimproto.AgentInfo, error) {
	resp, err := c.roundTrip(ctx, shimproto.Request{Op: shimproto.OpList})
	return resp.Agents, err
}

func (c *Client) Start(ctx context.Context, id string, spec shimproto.Spec) (shimproto.AgentInfo, error) {
	resp, err := c.roundTrip(ctx, shimproto.Request{Op: shimproto.OpStart, Agent: id, Spec: &spec})
	if err != nil {
		return shimproto.AgentInfo{}, err
	}
	return *resp.Agent, nil
}

func (c *Client) Signal(ctx context.Context, id, sig string) error {
	_, err := c.roundTrip(ctx, shimproto.Request{Op: shimproto.OpSignal, Agent: id, Signal: sig})
	return err
}

// Attachment is a live connection to one agent: its log from a point onward,
// and a way to write to its stdin.
type Attachment struct {
	// LastSeq is where catch-up ends; records after it are live.
	LastSeq int64
	Agent   shimproto.AgentInfo
	// Records is closed when the attachment ends; Err says why.
	Records <-chan shimproto.Record

	conn io.ReadWriteCloser
	wmu  sync.Mutex
	mu   sync.Mutex
	err  error
}

func (c *Client) Attach(ctx context.Context, id string, after int64) (*Attachment, error) {
	conn, err := c.Dial(ctx)
	if err != nil {
		return nil, err
	}
	stop := closeOnDone(ctx, conn)
	if err := writeJSONLine(conn, shimproto.Request{Op: shimproto.OpAttach, Agent: id, After: after}); err != nil {
		stop()
		conn.Close()
		return nil, err
	}
	r := bufio.NewReaderSize(conn, 64<<10)
	resp, err := readResponse(r)
	if err != nil {
		stop()
		conn.Close()
		return nil, err
	}
	ch := make(chan shimproto.Record, 256)
	a := &Attachment{LastSeq: resp.LastSeq, Agent: *resp.Agent, Records: ch, conn: conn}
	go func() {
		defer stop()
		defer close(ch)
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				a.setErr(err)
				return
			}
			var rec shimproto.Record
			if err := json.Unmarshal(line, &rec); err != nil {
				a.setErr(fmt.Errorf("shim: bad record: %w", err))
				return
			}
			ch <- rec
		}
	}()
	return a, nil
}

func (a *Attachment) setErr(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err == nil {
		a.err = err
	}
}

func (a *Attachment) Err() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.err
}

// Write sends one line to the agent's stdin. It must not contain a newline.
func (a *Attachment) Write(line []byte) error {
	a.wmu.Lock()
	defer a.wmu.Unlock()
	_, err := a.conn.Write(append(line, '\n'))
	return err
}

func (a *Attachment) Close() error { return a.conn.Close() }
