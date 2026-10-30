// Package shimproto is the wire format between the gero daemon and gero-shim,
// the process that owns agents inside an isolation.
//
// A connection carries one Request line and one Response line. For attach it
// then stays open: the shim streams Record lines, and every line the daemon
// sends is written verbatim to the agent's stdin.
//
// The shim is deliberately protocol-agnostic. It never parses what an agent
// says; it only keeps a durable, sequenced log of both directions. Everything
// that understands Claude Code or Codex lives in the daemon, so the shim — the
// one component whose upgrade means restarting every agent — rarely changes.
package shimproto

import "time"

// Version is reported by ping so the daemon can tell an outdated shim apart.
// 2 added `gero-shim tunnel` and `gero-shim sync`, which the credential
// proxies need; 3 lets the tunnel connect the daemon to the isolation's own
// loopback, which forwarded ports need; 4 opens 3030 (Linear's) as well.
const Version = "4"

// ProxiesVersion and ForwardsVersion are the first shims that serve the
// credential proxies and forwarded ports: an isolation whose image predates
// one goes without it until `gero iso upgrade`.
const (
	ProxiesVersion  = "2"
	ForwardsVersion = "3"
)

// Dir says which stream a Record came from.
type Dir string

const (
	DirOut Dir = "out" // a line the agent wrote to stdout
	DirErr Dir = "err" // a line the agent wrote to stderr
	DirIn  Dir = "in"  // a line written to the agent's stdin
	DirSys Dir = "sys" // shim lifecycle: started, exited, lost
)

type SysKind string

const (
	SysStarted SysKind = "started"
	SysExited  SysKind = "exited"
	// SysLost means the shim came back (container stop, reboot) and found the
	// agent recorded as running. The process is gone and its exit status is
	// unknown — to a reader of the log this is an exit that interrupted
	// whatever was in flight.
	SysLost SysKind = "lost"
)

// Record is one line of an agent's durable log. Seq is gapless and strictly
// increasing per agent, across every process the agent has ever had, so a
// reader can resume from the last Seq it processed.
type Record struct {
	Seq  int64     `json:"seq"`
	Time time.Time `json:"t"`
	Dir  Dir       `json:"dir"`
	Line string    `json:"line,omitempty"`
	Sys  *Sys      `json:"sys,omitempty"`
}

type Sys struct {
	Kind     SysKind  `json:"kind"`
	PID      int      `json:"pid,omitempty"`
	Argv     []string `json:"argv,omitempty"`
	ExitCode int      `json:"exit_code,omitempty"`
	Error    string   `json:"error,omitempty"`
}

// Spec is the process to run for an agent.
type Spec struct {
	Argv []string `json:"argv"`
	Dir  string   `json:"dir,omitempty"`
	// Env entries (KEY=VALUE) are appended to the shim's base agent
	// environment, so they win over it.
	Env []string `json:"env,omitempty"`
}

type Op string

const (
	OpPing   Op = "ping"
	OpStart  Op = "start"
	OpAttach Op = "attach"
	OpList   Op = "list"
	OpSignal Op = "signal"
)

type Request struct {
	Op    Op     `json:"op"`
	Agent string `json:"agent,omitempty"`
	Spec  *Spec  `json:"spec,omitempty"`
	// After is the last Seq the caller has; attach replays everything later.
	After int64 `json:"after,omitempty"`
	// Signal is TERM, INT or KILL, delivered to the agent's process group.
	Signal string `json:"signal,omitempty"`
}

type Response struct {
	OK     bool        `json:"ok"`
	Error  string      `json:"error,omitempty"`
	Agent  *AgentInfo  `json:"agent,omitempty"`
	Agents []AgentInfo `json:"agents,omitempty"`
	// LastSeq, on attach, is the newest Seq at the moment the attach began.
	// Records up to it are catch-up; anything later is live.
	LastSeq int64  `json:"last_seq,omitempty"`
	Version string `json:"version,omitempty"`
}

type AgentInfo struct {
	ID      string `json:"id"`
	Running bool   `json:"running"`
	PID     int    `json:"pid,omitempty"`
	LastSeq int64  `json:"last_seq"`
}

// The credential proxies' addresses inside an isolation. They are fixed, as
// in isolate, so the address an agent types never varies; `gero-shim tunnel`
// opens nothing outside this range.
const (
	TunnelPortLo = 3020
	TunnelPortHi = 3030
)

// PortVersion is the first shim whose tunnel opens port: the ones past
// 3029 came after the range first stopped there.
func PortVersion(port int) string {
	if port > 3029 {
		return "4"
	}
	return ProxiesVersion
}

// SyncRequest is what `gero-shim sync` makes true inside the isolation: the
// files gero manages there (skills, the proxies' environment, git's URL
// rewrite) and the ones it no longer wants. It is idempotent; the daemon
// sends the whole desired set every time.
type SyncRequest struct {
	Files  []SyncFile `json:"files"`
	Remove []string   `json:"remove,omitempty"`
}

type SyncFile struct {
	Path    string `json:"path"`
	Mode    uint32 `json:"mode"`
	Agent   bool   `json:"agent,omitempty"` // owned by the agent user, not root
	Content string `json:"content"`
}

type SyncResult struct {
	Written []string `json:"written,omitempty"`
	Removed []string `json:"removed,omitempty"`
}
