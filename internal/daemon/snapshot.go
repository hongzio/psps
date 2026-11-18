package daemon

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/store"
)

// An agent's snapshot is where its runtime stood after reading its shim log
// up to Seq: its fold driver's state, and what the runtime itself keeps of
// the log. Attaching again, the runtime takes it up and reads only the
// records after Seq, not the whole log, which for an agent that has run
// long is hundreds of megabytes through the isolation's exec pipe.
type rtSnapshot struct {
	Seq       int64           `json:"seq"`
	Driver    json.RawMessage `json:"driver"`
	SeenAt    time.Time       `json:"seen_at"`
	Answer    string          `json:"answer,omitempty"`
	Waits     string          `json:"waits,omitempty"`
	TurnSince time.Time       `json:"turn_since"`
}

// snapEvery is how many records go by, at most, between snapshots; a turn
// that ends keeps one too.
const snapEvery = 2000

// newDriver is a driver for the agent as a fresh one is made: what the log
// says it then learns.
func (r *runtime) newDriver(sessionID string) (driver.Driver, error) {
	drv, err := driver.New(r.agent.Kind, r.agent.ID, sessionID, r.agent.Options)
	if err != nil {
		return nil, err
	}
	drv.ForkFrom(r.agent.ForkOf)
	if r.agent.IsolationID == store.HostIsolation {
		drv.OnHost()
	}
	return drv, nil
}

// loadSnapshot is the agent's snapshot, when it has one a driver takes;
// else none (Seq 0), and its log is read from the start.
func (r *runtime) loadSnapshot() (rtSnapshot, error) {
	seq, data, err := r.m.store.Snapshot(r.agent.ID)
	if err != nil || data == nil {
		return rtSnapshot{}, err
	}
	var snap rtSnapshot
	if err := json.Unmarshal(data, &snap); err == nil && snap.Seq == seq {
		drv, err := driver.New(r.agent.Kind, r.agent.ID, "", driver.Options{})
		if err != nil {
			return rtSnapshot{}, err
		}
		if err = driver.Restore(drv, snap.Driver); err == nil {
			return snap, nil
		}
		if !errors.Is(err, driver.ErrSnapshot) {
			return rtSnapshot{}, err
		}
	}
	r.m.log.Printf("agent %s: its snapshot is not one this psps takes: reading its log from the start", r.agent.ID)
	return rtSnapshot{}, nil
}

// driversLocked are the agent's live driver and its fold, both from snap
// when it has one (loadSnapshot took it), else fresh, to read the log from
// its start.
func (r *runtime) driversLocked(snap rtSnapshot) (drv, fold driver.Driver, err error) {
	if drv, err = r.newDriver(r.agent.SessionID); err != nil {
		return nil, nil, err
	}
	// The fold is the same agent: a session id a fresh driver makes up is
	// the live one's.
	if fold, err = r.newDriver(drv.State().SessionID); err != nil {
		return nil, nil, err
	}
	if snap.Seq == 0 {
		return drv, fold, nil
	}
	if err := driver.Restore(drv, snap.Driver); err != nil {
		return nil, nil, err
	}
	if err := driver.Restore(fold, snap.Driver); err != nil {
		return nil, nil, err
	}
	return drv, fold, nil
}

// keepSnapshotLocked keeps where the runtime stands, unless that is where
// the last one kept it.
func (r *runtime) keepSnapshotLocked() {
	if r.fold == nil || r.lastSeen <= r.snapSeq {
		return
	}
	d, err := driver.Snapshot(r.fold)
	if err != nil {
		r.m.log.Printf("agent %s: snapshot: %v", r.agent.ID, err)
		return
	}
	data, err := json.Marshal(rtSnapshot{Seq: r.lastSeen, Driver: d, SeenAt: r.seenAt, Answer: r.answer, Waits: r.waits, TurnSince: r.turnSince})
	if err != nil {
		r.m.log.Printf("agent %s: snapshot: %v", r.agent.ID, err)
		return
	}
	if err := r.m.store.PutSnapshot(r.agent.ID, r.lastSeen, data); err != nil {
		r.m.log.Printf("agent %s: snapshot: %v", r.agent.ID, err)
		return
	}
	r.snapSeq = r.lastSeen
}
