package daemon

import (
	"context"
	"fmt"

	"github.com/hongzio/psps/internal/isolation"
	"github.com/hongzio/psps/internal/store"
)

// discardScript remounts each filesystem given with discard, so a block a
// file let go of goes back to the Mac at once: without it the VM's disks
// keep on the Mac every block ever written, however little is in them.
// container has no mount options, so root does this in the VM; it needs
// CAP_SYS_ADMIN, which a container that may have swap has.
const discardScript = `for p; do mount -o remount,discard "$p" || exit 1; done`

// discardLocked turns discard on in iso's container once each time it
// starts; one that cannot (no CAP_SYS_ADMIN) is not asked again until it
// starts again. Callers hold the isolation's lock with its container up.
func (m *Manager) discardLocked(ctx context.Context, iso store.Isolation) error {
	m.mu.Lock()
	done := m.discarded[iso.ID]
	m.discarded[iso.ID] = true
	m.mu.Unlock()
	if done {
		return nil
	}
	_, err := m.cli.RunAsRoot(ctx, iso.Container, "sh", "-c", discardScript, "psps-discard",
		"/", isolation.AgentHome, isolation.StateRoot(iso.Container))
	if err != nil {
		err = fmt.Errorf("isolation %s: discard: %w", iso.Name, err)
		m.log.Print(err)
		return err
	}
	m.log.Printf("isolation %s: discard on", iso.Name)
	return nil
}
