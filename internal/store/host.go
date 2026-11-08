package store

import "time"

// HostIsolation is the ID of the row that stands for the Mac itself: the
// admin agent runs there, outside every isolation. The row exists only so
// that agent has an isolation to belong to, as every agent does. It is not
// an isolation people see: Isolations and IsolationNamed leave it out, so no
// command that names an isolation reaches it. HostContainer is its
// container, which no container runtime has; the daemon reads it as the Mac.
const (
	HostIsolation = "host"
	HostContainer = "@host"
)

// EnsureHostIsolation makes the host's row if it is not there yet. Its
// fence is off: there is no network of its own to fence.
func (s *Store) EnsureHostIsolation() error {
	_, err := s.db.Exec(`INSERT INTO isolations (id, name, container, image, created_at, fence)
		VALUES (?, ?, ?, '', ?, 'off') ON CONFLICT(id) DO NOTHING`,
		HostIsolation, HostIsolation, HostContainer, ts(time.Now()))
	return err
}
