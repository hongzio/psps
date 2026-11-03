package daemon

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/hongzio/gero/internal/isolation"
	"github.com/hongzio/gero/internal/store"
)

// swapScript makes /swapfile $1 bytes and turns it on, or for 0 turns it
// off and removes it. One already on at that size is left as it is.
// container has no option for swap, so root does this in the VM.
const swapScript = `f=/swapfile
if grep -q "^$f " /proc/swaps; then
	[ "$(stat -c %s $f)" = "$1" ] && exit 0
	swapoff $f || exit 1
fi
rm -f $f
[ "$1" = 0 ] && exit 0
fallocate -l "$1" $f && chmod 600 $f && mkswap $f >/dev/null && swapon $f`

// swapSize is a swap as an isolation keeps it: "" for none (also "0"),
// refused when it is no size.
func swapSize(s string) (string, error) {
	s = strings.TrimSpace(s)
	if _, err := isolation.SwapBytes(s); err != nil {
		return "", badRequest(err)
	}
	if s == "0" {
		s = ""
	}
	return s, nil
}

// swapLocked turns on the swap iso asks for, once each time its container
// starts and when it changes; a VM that never had swap is left alone.
// Callers hold the isolation's lock with its container up.
func (m *Manager) swapLocked(ctx context.Context, iso store.Isolation) error {
	m.mu.Lock()
	have, known := m.swapped[iso.ID]
	m.mu.Unlock()
	if have == iso.Swap && (known || iso.Swap == "") {
		return nil
	}
	b, _ := isolation.SwapBytes(iso.Swap)
	_, err := m.cli.RunAsRoot(ctx, iso.Container, "sh", "-c", swapScript, "gero-swap", strconv.FormatUint(b, 10))
	// Not tried again until it changes or the container starts again: a
	// swap that cannot be had is not asked for every few seconds.
	m.mu.Lock()
	m.swapped[iso.ID] = iso.Swap
	m.mu.Unlock()
	if err != nil {
		err = fmt.Errorf("isolation %s: swap %s: %w", iso.Name, orNone(iso.Swap), err)
		m.log.Print(err)
		return err
	}
	m.log.Printf("isolation %s: swap %s", iso.Name, orNone(iso.Swap))
	return nil
}

// setSwap keeps an isolation's new swap and turns it on now if its
// container is running; one that is not gets it when it starts.
func (m *Manager) setSwap(ctx context.Context, id, swap string) error {
	st := m.isoLock(id)
	st.mu.Lock()
	defer st.mu.Unlock()
	iso, err := m.store.Isolation(id)
	if err != nil {
		return err
	}
	was := iso.Swap
	iso.Swap = swap
	if err := m.store.PutIsolation(iso); err != nil {
		return err
	}
	if state, err := m.cli.State(ctx, iso.Container); err != nil || state != "running" {
		return nil
	}
	// The VM has the old one on, whether or not this daemon turned it on.
	m.mu.Lock()
	m.swapped[id] = was
	m.mu.Unlock()
	return m.swapLocked(ctx, iso)
}
