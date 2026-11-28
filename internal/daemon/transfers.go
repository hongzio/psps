package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/isolation"
	"github.com/hongzio/psps/internal/proxy/askproxy"
	"github.com/hongzio/psps/internal/store"
)

// Transfers: an agent asks for a file to be moved from one isolation to
// another (an ask of kind transfer), along a route the operator allowed;
// the operator approves it, and the daemon copies it, as the agent user on
// both sides, into the other isolation's inbox (isolation/transfer.go).
// The agent may be in either isolation or in neither. What it asked for is
// read as it asks, its size and sha256 kept with the ask and shown to the
// operator; what is copied must still be that, or nothing lands and the
// ask stays open.

// transferTime is how long reading a file for an ask, or copying it, may
// take.
const transferTime = 10 * time.Minute

// postedTransfer is the file an agent asks to be moved, its isolations by
// name.
type postedTransfer struct {
	From   string `json:"from"`
	Path   string `json:"path"`
	To     string `json:"to"`
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// transferOf reads the file req names, refusing one that may not be moved.
func (m *Manager) transferOf(ctx context.Context, req *postedTransfer) (*store.AskTransfer, error) {
	if req == nil {
		return nil, badRequest(errors.New(`ask: a transfer needs "transfer": {"from": ISOLATION, "path": FILE, "to": ISOLATION}`))
	}
	fromName, toName := strings.TrimSpace(req.From), strings.TrimSpace(req.To)
	if fromName == "" || toName == "" || req.Path == "" {
		return nil, badRequest(errors.New(`ask: a transfer says "from" (an isolation), "path" (the file there) and "to" (the isolation it goes to)`))
	}
	from, err := m.store.IsolationNamed(fromName)
	if err != nil {
		return nil, badRequest(fmt.Errorf("ask: there is no isolation %s", fromName))
	}
	to, err := m.store.IsolationNamed(toName)
	if err != nil {
		return nil, badRequest(fmt.Errorf("ask: there is no isolation %s", toName))
	}
	if from.ID == to.ID {
		return nil, badRequest(fmt.Errorf("ask: %s is both ends; a transfer goes from one isolation to another", from.Name))
	}
	if ok, err := m.store.RouteAllowed(from.ID, to.ID); err != nil {
		return nil, err
	} else if !ok {
		return nil, httpError{http.StatusForbidden, fmt.Errorf("ask: your operator allows no transfers from %s to %s; they would allow it with `psps transfer allow %s %s`. Do not look for another way", from.Name, to.Name, from.Name, to.Name)}
	}
	if err := isolation.TransferPath(req.Path); err != nil {
		return nil, badRequest(fmt.Errorf("ask: %v", err))
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = path.Base(req.Path)
	}
	if err := isolation.TransferName(name); err != nil {
		return nil, badRequest(fmt.Errorf(`ask: %v; give it a "name"`, err))
	}
	want := strings.ToLower(strings.TrimSpace(req.SHA256))
	ctx, cancel := context.WithTimeout(ctx, transferTime)
	defer cancel()
	if _, err := m.ensureIsolation(ctx, from.ID); err != nil {
		return nil, fmt.Errorf("ask: isolation %s: %w", from.Name, err)
	}
	if from, err = m.store.Isolation(from.ID); err != nil {
		return nil, err
	}
	size, sum, err := m.cli.TransferStat(ctx, from.Container, req.Path)
	switch {
	case err != nil:
		return nil, httpError{http.StatusUnprocessableEntity, fmt.Errorf("ask: reading %s in %s: %v", req.Path, from.Name, err)}
	case size > askproxy.MaxTransfer:
		return nil, httpError{http.StatusRequestEntityTooLarge, fmt.Errorf("ask: %s is %d bytes; a transfer is at most %d", req.Path, size, askproxy.MaxTransfer)}
	case want != "" && want != sum:
		return nil, httpError{http.StatusConflict, fmt.Errorf("ask: %s's sha256 is %s, not the %s you gave", req.Path, sum, want)}
	}
	return &store.AskTransfer{From: from.ID, Path: req.Path, To: to.ID, Name: name, Size: size, SHA256: sum}, nil
}

// transferView is t as people read it.
func (m *Manager) transferView(t *store.AskTransfer) *api.Transfer {
	if t == nil {
		return nil
	}
	routed, _ := m.store.RouteAllowed(t.From, t.To)
	return &api.Transfer{From: m.isoName(t.From), Path: t.Path, To: m.isoName(t.To), Dest: isolation.InboxDir + "/" + t.Name,
		Size: t.Size, SHA256: t.SHA256, Routed: routed, CopiedAt: t.CopiedAt}
}

// runTransfer copies what open ask x asks to move, as the operator
// approved it.
func (m *Manager) runTransfer(x store.Ask) error {
	t := x.Transfer
	if t == nil {
		return fmt.Errorf("ask #%d names no file", x.ID)
	}
	m.transferMu.Lock()
	defer m.transferMu.Unlock()
	if ok, err := m.store.RouteAllowed(t.From, t.To); err != nil {
		return err
	} else if !ok {
		return httpError{http.StatusConflict, fmt.Errorf("transfers from %s to %s are not allowed any more: `psps transfer allow %s %s`, or decline it", m.isoName(t.From), m.isoName(t.To), m.isoName(t.From), m.isoName(t.To))}
	}
	ctx, cancel := context.WithTimeout(m.ctx, transferTime)
	defer cancel()
	var from, to store.Isolation
	for _, end := range []struct {
		id  string
		iso *store.Isolation
	}{{t.From, &from}, {t.To, &to}} {
		if _, err := m.ensureIsolation(ctx, end.id); err != nil {
			return fmt.Errorf("isolation %s: %w", m.isoName(end.id), err)
		}
		iso, err := m.store.Isolation(end.id)
		if err != nil {
			return fmt.Errorf("isolation %s: %w", m.isoName(end.id), err)
		}
		*end.iso = iso
	}
	part := fmt.Sprintf(".psps-transfer-%d", x.ID)
	err := m.cli.Transfer(ctx, from.Container, t.Path, to.Container, t.Name, part, t.Size, t.SHA256)
	switch {
	case errors.Is(err, isolation.ErrTransferChanged), errors.Is(err, isolation.ErrTransferExists):
		return httpError{http.StatusConflict, fmt.Errorf("nothing copied: %v", err)}
	case err != nil:
		return fmt.Errorf("nothing copied: %w", err)
	}
	m.log.Printf("ask #%d: %s:%s copied to %s:%s/%s, %d bytes", x.ID, from.Name, t.Path, to.Name, isolation.InboxDir, t.Name, t.Size)
	return m.store.TransferDone(x.ID, time.Now())
}

// --- the routes ---------------------------------------------------------------

func (m *Manager) routeView(r store.Route) api.Route {
	return api.Route{From: m.isoName(r.From), To: m.isoName(r.To), CreatedAt: r.CreatedAt}
}

// Routes are the directions files may be transferred in.
func (m *Manager) Routes() ([]api.Route, error) {
	rs, err := m.store.Routes()
	if err != nil {
		return nil, err
	}
	out := make([]api.Route, 0, len(rs))
	for _, r := range rs {
		out = append(out, m.routeView(r))
	}
	return out, nil
}

func (m *Manager) routeEnds(req api.Route) (store.Isolation, store.Isolation, error) {
	from, err := m.isolationNamed(req.From)
	if err != nil {
		return from, store.Isolation{}, err
	}
	to, err := m.isolationNamed(req.To)
	if err != nil {
		return from, to, err
	}
	if from.ID == to.ID {
		return from, to, badRequest(errors.New("a route goes from one isolation to another"))
	}
	return from, to, nil
}

// AllowRoute lets agents ask for files to be moved from req.From to req.To.
func (m *Manager) AllowRoute(req api.Route) (api.Route, error) {
	from, to, err := m.routeEnds(req)
	if err != nil {
		return api.Route{}, err
	}
	if _, err := m.store.AllowRoute(from.ID, to.ID); err != nil {
		return api.Route{}, err
	}
	m.log.Printf("transfers from %s to %s allowed", from.Name, to.Name)
	m.pokeChanges()
	return api.Route{From: from.Name, To: to.Name}, nil
}

// DenyRoute takes the route from req.From to req.To away: the transfers
// asked for along it wait until it is allowed again, or are declined.
func (m *Manager) DenyRoute(req api.Route) error {
	from, to, err := m.routeEnds(req)
	if err != nil {
		return err
	}
	had, err := m.store.DenyRoute(from.ID, to.ID)
	if err != nil {
		return err
	}
	if !had {
		return httpError{http.StatusNotFound, fmt.Errorf("transfers from %s to %s were not allowed", from.Name, to.Name)}
	}
	m.log.Printf("transfers from %s to %s no longer allowed", from.Name, to.Name)
	m.pokeChanges()
	return nil
}
