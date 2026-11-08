// Package api holds the request and response types of the daemon's HTTP API,
// shared by the daemon and every client (CLI, TUI, web).
package api

import (
	"time"

	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/event"
	"github.com/hongzio/psps/internal/proxy"
	"github.com/hongzio/psps/internal/store"
)

// Version is the release this psps was built as, set by the build with
// -ldflags "-X github.com/hongzio/psps/internal/api.Version=0.1.0" (the
// Homebrew formula does); any other build is "dev".
var Version = "dev"

type Health struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
	PID     int    `json:"pid"`
	// Web is the URL this daemon serves its web UI on, or empty with
	// WebError saying why it does not.
	Web      string `json:"web,omitempty"`
	WebError string `json:"web_error,omitempty"`
}

type Isolation struct {
	store.Isolation
	EnvKeys []string `json:"env_keys,omitempty"`
	State   string   `json:"state"`
	// FenceState is "verified", "off", or why agents cannot start.
	FenceState string `json:"fence_state"`
	// Health, for a running isolation, is HealthMemoryFull or HealthStuck
	// when it runs but cannot work, empty when it can or was not looked
	// into; HealthDetail says what was seen.
	Health       string `json:"health,omitempty"`
	HealthDetail string `json:"health_detail,omitempty"`
	// Proxies is "up" while the credential proxies' tunnel is open, or
	// what is keeping it down.
	Proxies string `json:"proxies,omitempty"`
	// Accounts names the account each agent CLI uses, by CLI; a CLI with
	// none uses a login made inside the isolation.
	Accounts map[driver.Kind]string `json:"accounts,omitempty"`
}

// What keeps a running isolation from working (Isolation.Health).
const (
	HealthMemoryFull = "memory full"
	HealthStuck      = "not answering"
)

type CreateIsolation struct {
	Name   string `json:"name"`
	CPUs   int    `json:"cpus,omitempty"`
	Memory string `json:"memory,omitempty"`
	// Swap is a swap file for the VM, like 8G; "" or "0" is none.
	Swap string `json:"swap,omitempty"`
	// Fence is "on" (default) or "off".
	Fence string `json:"fence,omitempty"`
}

// UpdateIsolation renames an isolation or resizes its VM; what it leaves out
// stays as it is. A new name changes nothing but the name: everything else
// knows the isolation by its ID, and agents keep running. A new size
// replaces the container (container cannot resize one): running agents end
// and start again on their next message; sessions and workspaces are kept.
// A new swap is turned on in the running VM, but the first one an
// isolation gets replaces the container too: swapon needs a capability
// psps gives only a container with swap.
type UpdateIsolation struct {
	Name   string  `json:"name,omitempty"`
	CPUs   *int    `json:"cpus,omitempty"`
	Memory *string `json:"memory,omitempty"`
	Swap   *string `json:"swap,omitempty"`
}

// SetEnv sets (non-nil) or removes (nil) isolation environment entries.
type SetEnv map[string]*string

type Agent struct {
	store.Agent
	// Isolation is the name of the isolation it runs in.
	Isolation string       `json:"isolation"`
	State     driver.State `json:"state"`
	// Problem is an infrastructure error (isolation down, shim unreachable)
	// that the agent's own status cannot express.
	Problem string `json:"problem,omitempty"`
	// Inbox is how many mails from other agents wait for it.
	Inbox int `json:"inbox,omitempty"`
	// Crons is how many of its crons are on.
	Crons int `json:"crons,omitempty"`
	// Todos is how many prompts are put aside for it.
	Todos int `json:"todos,omitempty"`
	// Team is the name of the team its line is in, TeamID its ID; the
	// sidebar groups a team's live agents.
	Team   string `json:"team,omitempty"`
	TeamID string `json:"team_id,omitempty"`
	// Icon is the one the operator picked for its line, if any.
	Icon *store.Icon `json:"icon,omitempty"`
	// Leads is the run of the team it leads, if it leads one.
	Leads *TeamBrief `json:"leads,omitempty"`
	// Host: it is the admin agent, on the Mac itself, in no isolation.
	Host bool `json:"host,omitempty"`
}

// Admin is admin mode: on while an admin agent runs on the Mac itself, in
// no isolation, as the user. Kind and Dir are its; Recent are the
// directories admin agents were started in, the latest first; Home is what
// ~ stands for on the Mac.
type Admin struct {
	On     bool        `json:"on"`
	Agent  string      `json:"agent,omitempty"`
	Kind   driver.Kind `json:"kind,omitempty"`
	Dir    string      `json:"dir,omitempty"`
	Recent []string    `json:"recent"`
	Home   string      `json:"home"`
}

// SetAdmin turns admin mode on, or starts its agent over in another
// directory or with the other CLI.
type SetAdmin struct {
	Kind driver.Kind `json:"kind"`
	Dir  string      `json:"dir"`
}

// AgentOrder is a sidebar list's lines, from the top.
type AgentOrder struct {
	Lines []string `json:"lines"`
}

// AgentOrders are both sidebar lists' lines, from the top, with the
// dividers ("divider:<id>") among them and, in the live one, the teams
// ("team:<id>"), every one of which is in Teams. A team's agents come in
// /v1/agents in its order, where it is in the live list.
type AgentOrders struct {
	Live     []string `json:"live"`
	Archived []string `json:"archived"`
	Teams    []Team   `json:"teams"`
}

// Team is a team as the sidebar draws it, with its lead's run if it has a
// lead.
type Team struct {
	store.Team
	Run *TeamBrief `json:"run,omitempty"`
}

// TeamBrief is what a team's head says of its run.
type TeamBrief struct {
	// Lead is the lead's line.
	Lead string `json:"lead"`
	// Status is a sentence: what the run is doing now.
	Status string `json:"status"`
	// Spent is the tokens (driver.Tokens.New) used of Budget, 0 for none.
	Spent  int64    `json:"spent"`
	Budget int64    `json:"budget"`
	Best   *float64 `json:"best,omitempty"`
	// Eval is the eval as it is now, Last the one before it, if any.
	Eval *EvalNow   `json:"eval,omitempty"`
	Last *EvalBrief `json:"last,omitempty"`
}

// EvalNow is a team's eval as it is now. State is one of:
//   - running: it runs (Started), as a job of Runner or in Isolation
//   - queued: its job waits on Runner, Place in line
//   - waiting: the round goes on (Waiting says on what); its end runs one
//   - due: the round ended at Since and no eval has started; a minute of
//     that is something wrong
//   - idle: nothing has changed since the last eval
//   - none: the team has no eval, or its run is stopped
type EvalNow struct {
	State string `json:"state"`
	// Number is the number it will be kept as.
	Number  int64     `json:"number,omitempty"`
	Why     string    `json:"why,omitempty"`
	Since   time.Time `json:"since,omitzero"`
	Started time.Time `json:"started,omitzero"`
	Place   int       `json:"place,omitempty"`
	Job     string    `json:"job,omitempty"`
	Runner  string    `json:"runner,omitempty"`
	// Isolation is where a command eval runs ("" in the lead's).
	Isolation string `json:"isolation,omitempty"`
	Note      string `json:"note,omitempty"`
	// Usual is the median of what the last few took, Limit its limit.
	Usual   time.Duration `json:"usual,omitempty"`
	Limit   time.Duration `json:"limit,omitempty"`
	Waiting string        `json:"waiting,omitempty"`
	// Stoppable: the operator can stop it (a job's).
	Stoppable bool `json:"stoppable,omitempty"`
}

// EvalBrief is an eval that ended, as a team's head shows it.
type EvalBrief struct {
	ID      int64         `json:"id"`
	At      time.Time     `json:"at"`
	Took    time.Duration `json:"took"`
	Score   *float64      `json:"score,omitempty"`
	NotRun  bool          `json:"not_run,omitempty"`
	NewBest bool          `json:"new_best,omitempty"`
	// Note is why it has no score.
	Note string `json:"note,omitempty"`
}

// PutTeam makes a team, or renames one.
type PutTeam struct {
	Name string `json:"name"`
}

// PutLead makes an agent its team's lead, or sets its terms; a term left
// out keeps what it was, or its default. Setting it again resumes a run
// psps stopped.
type PutLead struct {
	Agent       string  `json:"agent"`
	Budget      *int64  `json:"budget,omitempty"`
	MaxLive     *int    `json:"max_live,omitempty"`
	PlanCeiling *int    `json:"plan_ceiling,omitempty"`
	TurnMinutes *int    `json:"turn_minutes,omitempty"`
	Eval        *string `json:"eval,omitempty"`
	EvalDir     *string `json:"eval_dir,omitempty"`
	EvalMinutes *int    `json:"eval_minutes,omitempty"`
	// EvalIsolation names the isolation the eval runs in, on a copy of
	// EvalDir; "" is the lead's own.
	EvalIsolation *string `json:"eval_isolation,omitempty"`
	// EvalJob names the runner the eval is a job of instead; "" is none.
	EvalJob  *string  `json:"eval_job,omitempty"`
	Target   *float64 `json:"target,omitempty"`
	NoTarget bool     `json:"no_target,omitempty"`
	Patience *int     `json:"patience,omitempty"`
}

// TeamRun is a team with a lead, as its operator and its lead see it.
type TeamRun struct {
	Team store.Team `json:"team"`
	// Lead is the ID the lead goes by, LeadLine its line.
	Lead     string             `json:"lead"`
	LeadLine string             `json:"lead_line"`
	Settings store.LeadSettings `json:"settings"`
	// EvalIsolation is the name of Settings.EvalIsolation.
	EvalIsolation string    `json:"eval_isolation,omitempty"`
	Since         time.Time `json:"since"`
	Stopped       string    `json:"stopped,omitempty"`
	// TeamsOff is the lead's isolation while it has no team service: the
	// run is paused, and news waits for the lead.
	TeamsOff string `json:"teams_off,omitempty"`
	Status   string `json:"status"`
	// Spent is what the lead and its agents used since it became the lead.
	Spent driver.Tokens `json:"spent"`
	// Plan are the plan windows of the CLIs its agents run on.
	Plan       []PlanWindow `json:"plan,omitempty"`
	Evaluating bool         `json:"evaluating,omitempty"`
	Eval       *EvalNow     `json:"eval,omitempty"`
	// Evals are the latest, oldest first; Best the best of all, and Stale
	// how many have come since without a new one.
	Evals []store.Eval `json:"evals"`
	Best  *store.Eval  `json:"best,omitempty"`
	Stale int          `json:"stale"`
	// Agents are the lead's live agents; Archived counts the rest.
	Agents   []TeamAgent `json:"agents"`
	Archived int         `json:"archived"`
}

// PlanWindow is a plan window of the account a kind of CLI runs on in an
// isolation.
type PlanWindow struct {
	Kind driver.Kind `json:"kind"`
	driver.Limit
}

// TeamAgent is an agent a lead made, as the run shows it.
type TeamAgent struct {
	ID          string        `json:"id"`
	Role        string        `json:"role"`
	Kind        driver.Kind   `json:"kind"`
	Model       string        `json:"model,omitempty"`
	Status      event.Status  `json:"status"`
	TurnSince   *time.Time    `json:"turn_since,omitempty"`
	TurnMinutes int           `json:"turn_minutes,omitempty"`
	Spent       driver.Tokens `json:"spent"`
	// Waits is what it waits for the operator to approve.
	Waits string `json:"waits,omitempty"`
	// Answer is its last answer, in the view of it alone.
	Answer string `json:"answer,omitempty"`
}

// TeamAgentNew is an agent a lead makes.
type TeamAgentNew struct {
	Role        string      `json:"role"`
	Kind        driver.Kind `json:"kind"`
	Prompt      string      `json:"prompt"`
	Model       string      `json:"model,omitempty"`
	Effort      string      `json:"effort,omitempty"`
	Workdir     string      `json:"workdir,omitempty"`
	TurnMinutes int         `json:"turn_minutes,omitempty"`
}

// Mail is one agent's mail to another. From and To are the two agents as
// they are called now (as it was called, for a sender that is gone), with
// the isolations the mail went between. Sent: the agent whose mail this
// was asked for wrote it.
type Mail struct {
	ID            int64  `json:"id"`
	From          string `json:"from"`
	FromIsolation string `json:"from_isolation"`
	To            string `json:"to"`
	ToIsolation   string `json:"to_isolation"`
	Sent          bool   `json:"sent,omitempty"`
	// State is MailWaiting, MailDelivered or MailDiscarded.
	State       string     `json:"state"`
	Text        string     `json:"text"`
	CreatedAt   time.Time  `json:"created_at"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
	DiscardedAt *time.Time `json:"discarded_at,omitempty"`
}

// Mail states: in the recipient's inbox; handed to it as a turn; taken out
// of the inbox unread by the operator.
const (
	MailWaiting   = "waiting"
	MailDelivered = "delivered"
	MailDiscarded = "discarded"
)

// Cleared is how many mails a cleared inbox held.
type Cleared struct {
	Removed int `json:"removed"`
}

// UpdateAgent renames an agent, live or archived, or changes its own ID;
// what it leaves out stays as it is. Only what people call it changes: a
// running turn carries on.
type UpdateAgent struct {
	Name *string `json:"name,omitempty"`
	// Handle is the agent's own ID, unique among all agents; "" takes it
	// away, leaving the key psps gave it.
	Handle *string `json:"handle,omitempty"`
	// Parked parks an idle agent: it needs no look until it does something
	// again, which takes it out of being parked.
	Parked *bool `json:"parked,omitempty"`
	// Team puts its line, every session of it, in the team of that name,
	// made if there is none; "" takes it out of its team.
	Team *string `json:"team,omitempty"`
	// Icon gives its line, every session of it, that icon; one with no
	// name takes its icon away.
	Icon *store.Icon `json:"icon,omitempty"`
}

type CreateAgent struct {
	Isolation string      `json:"isolation"`
	Kind      driver.Kind `json:"kind"`
	Prompt    string      `json:"prompt"`
	Name      string      `json:"name,omitempty"`
	// Handle is an ID of the operator's for it, unique among all agents;
	// without one it gets an adjective-noun nobody else has.
	Handle string `json:"handle,omitempty"`
	Repo   string `json:"repo,omitempty"`
	// Workdir is where in the isolation it works, made if missing (Repo
	// cloned into it); ~ is the agent's home. Without one it gets a
	// workspace of its own under ~/work.
	Workdir string         `json:"workdir,omitempty"`
	Options driver.Options `json:"options"`
	// Team is the name of the team it starts in, made if there is none.
	Team string `json:"team,omitempty"`
}

// Dirs are the directories in an isolation a typed path could go on to:
// those in Parent whose names begin with what follows its last slash.
type Dirs struct {
	Parent string   `json:"parent"`
	Dirs   []string `json:"dirs"` // whole paths, each ending in a slash
	Home   string   `json:"home"` // what ~ stands for
}

type Message struct {
	Text string `json:"text"`
}

// Sent is what came of a message. Done, for a message that was one of
// psps's commands, is what psps did instead of sending it; Agent, after
// /new, is the key of the agent that took its place.
type Sent struct {
	Done  string `json:"done,omitempty"`
	Agent string `json:"agent,omitempty"`
}

// Commands are what may be typed in a message to an agent, for a message
// box to offer: psps's own first, then its CLI's.
type Commands struct {
	Commands []Command `json:"commands"`
}

type Command struct {
	driver.Command
	// Psps: psps does it itself, and the agent is not sent it.
	Psps bool `json:"psps,omitempty"`
}

// Files are the files in an agent's workspace, by their paths in it, for
// a message to name one.
type Files struct {
	Files []string `json:"files"`
	// Truncated: there are more than these.
	Truncated bool `json:"truncated,omitempty"`
}

// Branch is the git branch an agent's workspace is on: a commit when it is
// on none, "" when it is no repository.
type Branch struct {
	Branch string `json:"branch"`
}

// Rewind replaces a message the agent got: Event is that message's event
// ID; the agent goes back to before it and gets Text instead.
type Rewind struct {
	Event int64  `json:"event"`
	Text  string `json:"text"`
}

// Crons are an agent's crons, with the time zone their schedules are read
// in: the Mac's.
type Crons struct {
	Zone  string       `json:"zone"`
	Crons []store.Cron `json:"crons"`
}

// ControlView is what the control service opens to an isolation's agents:
// its env's actions, on the agents of the isolations it reaches (by name,
// its own first), and how many agents they made through it are live, of
// how many they may.
type ControlView struct {
	Env     string   `json:"env"`
	Allow   []string `json:"allow"`
	Reach   []string `json:"reach"`
	Live    int      `json:"live"`
	MaxLive int      `json:"max_live"`
	// Elsewhere is the approvals of an agent made in another isolation:
	// ask, or auto when the operator gave it and the maker has as much.
	Elsewhere string `json:"elsewhere_approvals"`
}

// ControlExec is what a command an agent ran in an isolation through the
// control service printed (stdout and stderr together, the end of it when
// Cut), and its exit code.
type ControlExec struct {
	Exit   int    `json:"exit"`
	Output string `json:"output"`
	Cut    bool   `json:"cut,omitempty"`
}

// PutCron sets a cron of an agent's: a cron expression or "@every 30m" for
// Schedule, the message, and store.CronSkip or store.CronWait for what a
// run that finds the agent busy does. What is left out stays as it is; a
// new cron is on, and skips.
type PutCron struct {
	Schedule *string `json:"schedule,omitempty"`
	Prompt   *string `json:"prompt,omitempty"`
	IfBusy   *string `json:"if_busy,omitempty"`
	Enabled  *bool   `json:"enabled,omitempty"`
	// Once: sent once, then removed; it waits if the agent is busy.
	Once *bool `json:"once,omitempty"`
	// Compact: each run first sends the agent /compact, and the message
	// once it is idle after it.
	Compact *bool `json:"compact,omitempty"`
}

// AddTodo puts a prompt aside for an agent.
type AddTodo struct {
	Text string `json:"text"`
	// Auto sends it by itself, oldest first, one each time a turn of the
	// agent's ends in idle.
	Auto bool `json:"auto,omitempty"`
}

// PutTodo changes a todo; what is left out stays as it is.
type PutTodo struct {
	Auto *bool `json:"auto,omitempty"`
}

type Decision struct {
	Decision driver.Decision `json:"decision"`
	// Answers answer a question (an approval of kind "question"): each
	// question's text to an option's label, labels joined with ", " where
	// several may be picked, or an answer of the person's own. Accepting
	// one needs every question answered.
	Answers map[string]string `json:"answers,omitempty"`
}

type Error struct {
	Error string `json:"error"`
}

// Service describes one credential proxy, enough for a client to build its
// forms and flags.
type Service struct {
	Name     string        `json:"name"`
	Summary  string        `json:"summary"`
	Ports    []int         `json:"ports"`
	Fields   []proxy.Field `json:"fields"`
	Settings []proxy.Field `json:"settings"`
	// Secrets are the credential fields `psps env secret` asks for; Login
	// means a browser login writes them instead.
	Secrets []string `json:"secrets,omitempty"`
	Login   bool     `json:"login,omitempty"`
	// Replaces means turning it on or off for an isolation, or changing
	// one of its envs, replaces the isolation's container (share): running
	// agents end and start again on their next message.
	Replaces bool `json:"replaces,omitempty"`
	// Forwards means its envs are ports of the isolation's on the Mac's
	// 127.0.0.1 (port), which an Exposure lists with what listens.
	Forwards bool `json:"forwards,omitempty"`
	// OneEnv means an isolation has it with exactly one env (control).
	OneEnv bool `json:"one_env,omitempty"`
}

// Env is an env definition as clients see it: never the secret, only
// whether it is set.
type Env struct {
	store.Env
	// MissingSecrets names the credential fields not yet stored; empty when
	// the env's secret is complete or the service keeps none.
	MissingSecrets []string `json:"missing_secrets,omitempty"`
	SecretError    string   `json:"secret_error,omitempty"`
	// CredentialElsewhere: the env takes its credential from somewhere
	// other than a stored secret (git's token command), so none is missing.
	CredentialElsewhere bool     `json:"credential_elsewhere,omitempty"`
	ExposedTo           []string `json:"exposed_to,omitempty"`
	Warnings            []string `json:"warnings,omitempty"`
	// Status is what the service knows of the env beyond its definition
	// (a skill from GitHub: whether there is an update).
	Status *proxy.EnvStatus `json:"status,omitempty"`
}

// Exposure is one service's state for one isolation.
type Exposure struct {
	Service string `json:"service"`
	Enabled bool   `json:"enabled"`
	store.Exposure
	// Missing names exposed envs the registry no longer has.
	Missing []string `json:"missing,omitempty"`
	// Forwards are a forwarding service's ports; Error says why one is not
	// listening on the Mac.
	Forwards []Forward `json:"forwards,omitempty"`
	// Warning is what turning it on costs, said once it is on: a team's
	// eval its isolation runs is refused while it has the service.
	Warning string `json:"warning,omitempty"`
}

type Forward struct {
	proxy.Forward
	// URL is where it is opened on the Mac.
	URL   string `json:"url"`
	Error string `json:"error,omitempty"`
}

// Expose turns a service on for an isolation, or changes what it serves.
// A nil Envs keeps the current list.
type Expose struct {
	Envs     []string    `json:"envs"`
	Default  string      `json:"default_env,omitempty"`
	Settings proxy.Patch `json:"settings"`
}

// ChoicesRequest asks a service for a Lookup field's choices. Env names a
// stored env whose definition and secret are the starting point; Set and
// Secret are what a form has typed so far, which win.
type ChoicesRequest struct {
	Env    string            `json:"env,omitempty"`
	Set    map[string]any    `json:"set,omitempty"`
	Secret map[string]string `json:"secret,omitempty"`
}

type Check struct {
	Output string `json:"output"`
	Error  string `json:"error,omitempty"`
}

// Severities of a verify check. An isolation check failing means a
// guarantee is broken; a config check failing means something is set up
// wrong or not at all.
const (
	SevIsolation = "isolation"
	SevConfig    = "config"
)

// VerifyCheck is one measured guarantee.
type VerifyCheck struct {
	Check    string `json:"check"`
	OK       bool   `json:"ok"`
	Severity string `json:"severity"`
	Expected string `json:"expected,omitempty"`
	Got      string `json:"got"`
	Detail   string `json:"detail,omitempty"`
}

// Killed is `psps iso kill`: the Mac's processes that ended an isolation's
// container, its runtime helper and its VM; none when it was not running.
type Killed struct {
	Processes []int `json:"processes"`
}

// Verify is `psps iso verify`: an isolation's guarantees, measured now.
type Verify struct {
	Isolation string        `json:"isolation"`
	Checks    []VerifyCheck `json:"checks"`
}

// Exit codes of `psps iso verify`, as isolate's.
const (
	VerifyOK        = 0
	VerifyIsolation = 1
	VerifyConfig    = 2
)

func (v Verify) ExitCode() int {
	code := VerifyOK
	for _, c := range v.Checks {
		switch {
		case c.OK:
		case c.Severity == SevIsolation:
			return VerifyIsolation
		default:
			code = VerifyConfig
		}
	}
	return code
}

// CLI is an agent CLI in an isolation as an agent there finds it: its
// account and the isolation's environment applied.
type CLI struct {
	Tool     driver.Kind `json:"tool"`
	Version  string      `json:"version,omitempty"`
	LoggedIn bool        `json:"logged_in"`
	// Method is how it is logged in: "Claude subscription",
	// "CLAUDE_CODE_OAUTH_TOKEN", "ChatGPT", "API key", …
	Method string `json:"method,omitempty"`
	Email  string `json:"email,omitempty"`
	// Account is the account the isolation uses for it, if any.
	Account string `json:"account,omitempty"`
	// Error is why the state could not be read.
	Error string `json:"error,omitempty"`
	// Login is the login in progress, or the one that just ended.
	Login *CLILogin `json:"login,omitempty"`
}

// CLI login states.
const (
	CLILoginStarting  = "starting"  // the CLI has not said where to go yet
	CLILoginOpen      = "open"      // waiting for the browser
	CLILoginVerifying = "verifying" // Claude's code was handed over
	CLILoginDone      = "done"
	CLILoginFailed    = "failed"
)

// CLILogin is an agent CLI's own login, run by the daemon inside the
// isolation. Claude's ends on a page showing a code to paste back
// (NeedsCode); Codex's asks for Code to be typed at URL and notices by
// itself.
type CLILogin struct {
	Tool      driver.Kind `json:"tool"`
	State     string      `json:"state"`
	URL       string      `json:"url,omitempty"`
	Code      string      `json:"code,omitempty"`
	NeedsCode bool        `json:"needs_code,omitempty"`
	// Message is the CLI's last words once the login ended.
	Message string    `json:"message,omitempty"`
	Expires time.Time `json:"expires"`
}

// CLICode is the code Claude's login page shows.
type CLICode struct {
	Code string `json:"code"`
}

// AccountKind is a kind of agent CLI credential an Account can hold: one
// that stays valid wherever it is used at once, so any number of isolations
// can share it. (A ChatGPT login cannot: its refresh token is single-use,
// so Codex logs in inside each isolation.)
type AccountKind struct {
	Name string      `json:"name"`
	Tool driver.Kind `json:"tool"`
	// Env is the variable agents start with.
	Env     string `json:"env"`
	Summary string `json:"summary"`
	// Prefix is what every value of the kind starts with.
	Prefix string `json:"prefix,omitempty"`
}

var AccountKinds = []AccountKind{
	{Name: "claude-token", Tool: driver.Claude, Env: "CLAUDE_CODE_OAUTH_TOKEN", Prefix: "sk-ant-oat",
		Summary: "a token from `claude setup-token`: your Claude subscription, valid for a year"},
	{Name: "anthropic-api-key", Tool: driver.Claude, Env: "ANTHROPIC_API_KEY", Prefix: "sk-ant-api",
		Summary: "an Anthropic Console API key, billed per use"},
}

func AccountKindNamed(name string) (AccountKind, bool) {
	for _, k := range AccountKinds {
		if k.Name == name {
			return k, true
		}
	}
	return AccountKind{}, false
}

// Account is an account as clients see it: never the secret.
type Account struct {
	store.Account
	Tool driver.Kind `json:"tool"`
	// UsedBy are the isolations that use it.
	UsedBy []string `json:"used_by,omitempty"`
	// SecretError is why its secret cannot be read; empty when it can.
	SecretError string `json:"secret_error,omitempty"`
}

// PutAccount adds an account (Kind and Secret required) or replaces an
// account's secret.
type PutAccount struct {
	Kind   string `json:"kind,omitempty"`
	Secret string `json:"secret"`
}

// UseAccount sets the account an isolation's CLI uses; "" goes back to a
// login inside the isolation.
type UseAccount struct {
	Account string `json:"account"`
}

// LoginStart begins a browser login for a service whose credential is one.
// Redirect is a loopback address the caller may be listening on; empty
// picks one nobody is, and the operator pastes back where the browser
// landed.
type LoginStart struct {
	Redirect string `json:"redirect,omitempty"`
}

type Login struct {
	ID       string    `json:"id"`
	URL      string    `json:"url"`
	Redirect string    `json:"redirect"`
	Expires  time.Time `json:"expires"`
}

// LoginFinish carries the address the browser landed on, or its query.
type LoginFinish struct {
	Callback string `json:"callback"`
}

// EventsTail is the end of an agent's history (GET .../events?tail=N):
// Earlier is whether there are events before these.
type EventsTail struct {
	Events  []event.Event `json:"events"`
	Earlier bool          `json:"earlier"`
}
