package daemon

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hongzio/gero/internal/isolation"
	"github.com/hongzio/gero/internal/proxy"
)

// mountsFor is the Mac's folders an isolation's container is to have: what
// its exposed shares name, each checked as it is now. Missing says which
// were left out, and why.
func (m *Manager) mountsFor(id string) (mounts []isolation.Mount, missing []string) {
	for _, svc := range m.registry().All() {
		mt, ok := svc.(proxy.Mounter)
		if !ok {
			continue
		}
		v, err := m.viewFor(id, svc)
		if err != nil {
			missing = append(missing, svc.Name()+": "+err.Error())
			continue
		}
		if !v.Enabled {
			continue
		}
		ms, gone := mt.Mounts(v)
		mounts = append(mounts, ms...)
		missing = append(missing, gone...)
	}
	return mounts, missing
}

func (m *Manager) logMissing(name string, missing []string) {
	for _, n := range missing {
		m.log.Printf("isolation %s: share %s is left out", name, n)
	}
}

func sameMounts(a, b []isolation.Mount) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(l []isolation.Mount) []string {
		out := make([]string, len(l))
		for i, x := range l {
			out[i] = fmt.Sprintf("%s\x00%s\x00%v", x.Target, x.Source, x.ReadOnly)
		}
		sort.Strings(out)
		return out
	}
	ka, kb := key(a), key(b)
	for i := range ka {
		if ka[i] != kb[i] {
			return false
		}
	}
	return true
}

func describeMounts(l []isolation.Mount) string {
	if len(l) == 0 {
		return "none"
	}
	var out []string
	for _, x := range l {
		mode := "read-write"
		if x.ReadOnly {
			mode = "read-only"
		}
		out = append(out, fmt.Sprintf("%s (%s, %s)", x.Target, x.Source, mode))
	}
	return strings.Join(out, ", ")
}

// remount gives an isolation's container the folders its shares name now.
// container mounts a folder only when it starts a container, so a change
// replaces the container the way a resize does: the volumes, and every
// session and workspace, are kept; running agents end and start again on
// their next message. A container that will not start with the new
// folders (macOS keeping the runtime out of one) is put back with the old.
func (m *Manager) remount(ctx context.Context, id string) error {
	iso, err := m.store.Isolation(id)
	if err != nil {
		return err
	}
	want, missing := m.mountsFor(id)
	st := m.isoLock(id)
	st.mu.Lock()
	state, err := m.cli.State(ctx, iso.Container)
	if err != nil || state == "missing" {
		// Nothing to replace: it is created with them when it next starts.
		st.mu.Unlock()
		return err
	}
	have, err := m.cli.Mounts(ctx, iso.Container)
	if err != nil {
		st.mu.Unlock()
		return err
	}
	if sameMounts(have, want) {
		st.mu.Unlock()
		return nil
	}
	m.logMissing(iso.Name, missing)
	if !m.cli.ImageExists(ctx, iso.Image) {
		image, err := m.cli.EnsureImage(ctx, nil)
		if err != nil {
			st.mu.Unlock()
			return err
		}
		iso.Image = image
		m.store.PutIsolation(iso)
	}
	err = m.cli.Remove(ctx, iso.Container)
	if err == nil {
		if err = m.cli.Create(ctx, id, iso.Image, iso.CPUs, iso.Memory, iso.Network, want); err != nil {
			m.cli.Remove(ctx, iso.Container)
			if berr := m.cli.Create(ctx, id, iso.Image, iso.CPUs, iso.Memory, iso.Network, have); berr != nil {
				err = fmt.Errorf("%w; starting it again with its old folders failed too: %v", err, berr)
			} else {
				err = fmt.Errorf("%w; it is back with the folders it had", err)
			}
		}
	}
	st.healthy = time.Time{}
	st.mu.Unlock()
	m.markFenceStale(id)
	if err == nil {
		m.log.Printf("isolation %s: restarted with its shared folders: %s", iso.Name, describeMounts(want))
	}
	if _, uerr := m.ensureIsolation(ctx, id); err == nil {
		err = uerr
	}
	return err
}

// remountAll remounts each isolation of ids, saying for each one that could
// not be what it kept.
func (m *Manager) remountAll(ctx context.Context, ids []string) []string {
	var problems []string
	for _, id := range ids {
		if err := m.remount(ctx, id); err != nil {
			problems = append(problems, fmt.Sprintf("isolation %s could not be restarted with it: %v", m.isoName(id), err))
		}
	}
	return problems
}
