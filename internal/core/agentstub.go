package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Agents: the same program running on another server, in "agent" mode. It connects OUT to the
// panel (no port is opened on the database machine), asks for work, runs the backup next to the
// data and streams it straight to the destination. The panel keeps the catalog, the schedule and
// the keys.

// endpointAgent returns the agent a target belongs to (0 = this machine).
func endpointAgent(t *Endpoint) int64 {
	var c struct {
		Agent int64 `json:"agent"`
	}
	json.Unmarshal(t.Cfg, &c)
	return c.Agent
}

// TaskEndpoint is a target or destination as sent to an agent, with its secret in clear (the link
// to the panel must be HTTPS).
type TaskEndpoint struct {
	ID     int64           `json:"id"`
	Name   string          `json:"name"`
	Kind   string          `json:"kind"`
	Cfg    json.RawMessage `json:"cfg"`
	Secret string          `json:"secret"`
}

// AgentTask is everything an agent needs to run one backup.
type AgentTask struct {
	ID          string       `json:"id"`
	JobID       int64        `json:"jobId"`
	Attempt     int          `json:"attempt"`
	Backup      *Backup      `json:"backup"`
	Plan        *Plan        `json:"plan"`
	Target      TaskEndpoint `json:"target"`
	Destination TaskEndpoint `json:"destination"`
	DataKey     string       `json:"dataKey,omitempty"` // hex; empty = not encrypted
	Wrapped     string       `json:"wrapped,omitempty"`
	Parent      *Backup      `json:"parent,omitempty"` // physical: the backup to build an incremental on
}

type AgentResult struct {
	TaskID   string    `json:"taskId"`
	Manifest *Manifest `json:"manifest,omitempty"`
	Error    string    `json:"error,omitempty"`
	NoRetry  bool      `json:"noRetry,omitempty"`
}

type liveTask struct {
	task      *AgentTask
	agent     int64
	picked    bool
	cancelled bool
	lastSeen  time.Time
	report    func(any)
	result    chan AgentResult
}

type agentHub struct {
	mu    sync.Mutex
	tasks map[string]*liveTask
	wake  map[int64]chan struct{}
}

var agents = &agentHub{tasks: map[string]*liveTask{}, wake: map[int64]chan struct{}{}}

func (h *agentHub) waker(agent int64) chan struct{} {
	if h.wake[agent] == nil {
		h.wake[agent] = make(chan struct{}, 1)
	}
	return h.wake[agent]
}

func agentTokenHash(tok string) string {
	s := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(s[:])
}

// AgentByToken finds the agent a token belongs to and notes that it is alive.
func (a *App) AgentByToken(tok, hostname, version string) (int64, bool) {
	var id int64
	if a.Store.DB.QueryRow(`SELECT id FROM agents WHERE token_hash=?`, agentTokenHash(tok)).Scan(&id) != nil {
		return 0, false
	}
	if hostname != "" {
		a.Store.DB.Exec(`UPDATE agents SET last_seen=?, hostname=?, version=? WHERE id=?`, now(), hostname, version, id)
	} else {
		a.Store.DB.Exec(`UPDATE agents SET last_seen=? WHERE id=?`, now(), id)
	}
	return id, true
}

// AgentPoll waits up to the context's deadline for a task for this agent.
func (a *App) AgentPoll(ctx context.Context, agent int64) *AgentTask {
	for {
		agents.mu.Lock()
		for _, lt := range agents.tasks {
			if lt.agent == agent && !lt.picked && !lt.cancelled {
				lt.picked, lt.lastSeen = true, time.Now()
				agents.mu.Unlock()
				return lt.task
			}
		}
		wake := agents.waker(agent)
		agents.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil
		case <-wake:
		}
	}
}

// AgentProgress records a progress report and tells the agent whether to stop.
func (a *App) AgentProgress(agent int64, taskID string, progress json.RawMessage) (cancel bool) {
	agents.mu.Lock()
	lt := agents.tasks[taskID]
	if lt == nil || lt.agent != agent {
		agents.mu.Unlock()
		return true // the panel no longer waits for this task (restarted, cancelled, timed out)
	}
	lt.lastSeen = time.Now()
	cancel, report := lt.cancelled, lt.report
	agents.mu.Unlock()
	if len(progress) > 0 && string(progress) != "null" {
		var p Progress
		if json.Unmarshal(progress, &p) == nil {
			report(p)
		}
	}
	return cancel
}

func (a *App) AgentFinish(agent int64, res AgentResult) {
	agents.mu.Lock()
	lt := agents.tasks[res.TaskID]
	agents.mu.Unlock()
	if lt == nil || lt.agent != agent {
		return
	}
	select {
	case lt.result <- res:
	default:
	}
}

var (
	agentPickup  = envSeconds("BM_AGENT_PICKUP_SECONDS", 90)   // how long a task may wait for its agent to fetch it
	agentSilence = envSeconds("BM_AGENT_SILENCE_SECONDS", 120) // how long a running agent may say nothing
)

func envSeconds(name string, def int) time.Duration {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return time.Duration(def) * time.Second
}

func init() { runOnAgentHook = runOnAgentImpl }

var runOnAgentHook func(a *App, ctx context.Context, agent int64, j *Job, b *Backup, p *Plan, t *Endpoint, report func(any)) (*Manifest, []byte, error)

func (a *App) runOnAgent(ctx context.Context, agent int64, j *Job, b *Backup, p *Plan, t *Endpoint, report func(any)) (*Manifest, []byte, error) {
	return runOnAgentHook(a, ctx, agent, j, b, p, t, report)
}

func runOnAgentImpl(a *App, ctx context.Context, agent int64, j *Job, b *Backup, p *Plan, t *Endpoint, report func(any)) (*Manifest, []byte, error) {
	var name string
	var seen int64
	if a.Store.DB.QueryRow(`SELECT name,last_seen FROM agents WHERE id=?`, agent).Scan(&name, &seen) != nil {
		return nil, nil, fmt.Errorf("the agent of %q was deleted: %w", t.Name, ErrNoRetry)
	}
	d, err := a.Store.Endpoint("destinations", b.DestinationID)
	if err != nil {
		return nil, nil, fmt.Errorf("destination not found: %w", ErrNoRetry)
	}
	tsec, err1 := a.Keys.Open(t.secret)
	dsec, err2 := a.Keys.Open(d.secret)
	if err1 != nil || err2 != nil {
		return nil, nil, errors.New("cannot read a stored secret (master key changed?)")
	}
	task := &AgentTask{ID: fmt.Sprintf("%s-%d", b.ID, j.Attempt), JobID: j.ID, Attempt: j.Attempt, Backup: b, Plan: p,
		Target:      TaskEndpoint{ID: t.ID, Name: t.Name, Kind: t.Kind, Cfg: t.Cfg, Secret: string(tsec)},
		Destination: TaskEndpoint{ID: d.ID, Name: d.Name, Kind: d.Kind, Cfg: d.Cfg, Secret: string(dsec)}}
	var wrapped []byte
	if !p.Spec.NoEncrypt {
		key, w, err := a.Keys.NewDataKey()
		if err != nil {
			return nil, nil, err
		}
		wrapped, task.DataKey, task.Wrapped = w, hex.EncodeToString(key), hex.EncodeToString(w)
	}
	if p.Spec.Method == "physical" && p.Spec.IncrementalsPerFull > 0 {
		if tip, n := a.chainTip(p.ID, b.DestinationID); tip != nil && n < p.Spec.IncrementalsPerFull {
			task.Parent = tip
		}
	}
	lt := &liveTask{task: task, agent: agent, report: report, result: make(chan AgentResult, 1), lastSeen: time.Now()}
	agents.mu.Lock()
	agents.tasks[task.ID] = lt
	select {
	case agents.waker(agent) <- struct{}{}:
	default:
	}
	agents.mu.Unlock()
	defer func() { agents.mu.Lock(); delete(agents.tasks, task.ID); agents.mu.Unlock() }()
	report(Progress{Phase: "waiting for agent " + name})

	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	queued := time.Now()
	for {
		select {
		case res := <-lt.result:
			if res.Error != "" {
				err := fmt.Errorf("agent %s: %s", name, res.Error)
				if res.NoRetry {
					err = fmt.Errorf("%w: %w", err, ErrNoRetry)
				}
				return nil, nil, err
			}
			if res.Manifest == nil || res.Manifest.ID != b.ID {
				return nil, nil, fmt.Errorf("agent %s sent back an unexpected result", name)
			}
			return res.Manifest, wrapped, nil
		case <-ctx.Done():
			agents.mu.Lock()
			lt.cancelled = true
			agents.mu.Unlock()
			// give the agent a moment to hear about it and stop its tool before the job is closed
			select {
			case <-lt.result:
			case <-time.After(8 * time.Second):
			}
			return nil, nil, ctx.Err()
		case <-tick.C:
			agents.mu.Lock()
			picked, last := lt.picked, lt.lastSeen
			agents.mu.Unlock()
			if !picked && time.Since(queued) > agentPickup {
				return nil, nil, fmt.Errorf("agent %s did not pick up the backup (is it running? last seen %s)", name, agoText(seen))
			}
			if picked && time.Since(last) > agentSilence {
				return nil, nil, fmt.Errorf("agent %s went silent in the middle of the backup", name)
			}
		}
	}
}

func agoText(unix int64) string {
	if unix == 0 {
		return "never"
	}
	return time.Since(time.Unix(unix, 0)).Round(time.Second).String() + " ago"
}

// RunAgentTask is the agent side: it runs one task with a throwaway local catalog.
func (a *App) RunAgentTask(ctx context.Context, t *AgentTask, report func(any)) AgentResult {
	res := AgentResult{TaskID: t.ID}
	fail := func(err error) AgentResult {
		res.Error, res.NoRetry = strings.TrimSuffix(err.Error(), ": "+ErrNoRetry.Error()), errors.Is(err, ErrNoRetry)
		return res
	}
	if t.Backup == nil || t.Plan == nil {
		return fail(fmt.Errorf("incomplete task: %w", ErrNoRetry))
	}
	for table, e := range map[string]TaskEndpoint{"targets": t.Target, "destinations": t.Destination} {
		sealed, err := a.Keys.Seal([]byte(e.Secret))
		if err != nil {
			return fail(err)
		}
		if _, err := a.Store.DB.Exec(`INSERT OR REPLACE INTO `+table+`(id,name,kind,cfg,secret,created_at) VALUES(?,?,?,?,?,?)`, e.ID, e.Name, e.Kind, string(e.Cfg), sealed, now()); err != nil {
			return fail(err)
		}
	}
	rc := &RemoteContext{Parent: t.Parent}
	if t.DataKey != "" {
		rc.DataKey, _ = hex.DecodeString(t.DataKey)
		rc.Wrapped, _ = hex.DecodeString(t.Wrapped)
		if len(rc.DataKey) != 32 {
			return fail(fmt.Errorf("the task carries a bad key: %w", ErrNoRetry))
		}
	}
	a.Remote = rc
	p := t.Plan
	p.Spec.Method = t.Backup.Method
	if err := p.Spec.Normalize(); err != nil {
		return fail(fmt.Errorf("%w: %w", err, ErrNoRetry))
	}
	m, _, err := a.ExecuteLocal(ctx, &Job{ID: t.JobID, Attempt: t.Attempt, BackupID: t.Backup.ID}, t.Backup, p, report)
	if err != nil {
		if st, _, serr := a.storage(t.Backup.DestinationID); serr == nil { // leave no partial files behind
			c, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			st.DeletePrefix(c, t.Backup.ID)
			cancel()
		}
		return fail(err)
	}
	res.Manifest = m
	return res
}

// ---- agents in the catalog ----

type AgentInfo struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Hostname string `json:"hostname"`
	Version  string `json:"version"`
	LastSeen int64  `json:"lastSeen"`
	Online   bool   `json:"online"`
}

func (a *App) Agents() ([]AgentInfo, error) {
	rows, err := a.Store.DB.Query(`SELECT id,name,hostname,version,last_seen FROM agents ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentInfo{}
	for rows.Next() {
		var x AgentInfo
		rows.Scan(&x.ID, &x.Name, &x.Hostname, &x.Version, &x.LastSeen)
		x.Online = now()-x.LastSeen < 60
		out = append(out, x)
	}
	return out, rows.Err()
}

// NewAgent registers an agent and returns its token (shown once; only the hash is kept).
func (a *App) NewAgent(name string) (int64, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, "", errors.New("give the agent a name")
	}
	raw := make([]byte, 24)
	if _, err := randRead(raw); err != nil {
		return 0, "", err
	}
	tok := "bma_" + hex.EncodeToString(raw)
	r, err := a.Store.DB.Exec(`INSERT INTO agents(name,token_hash,created_at) VALUES(?,?,?)`, name, agentTokenHash(tok), now())
	if err != nil {
		return 0, "", err
	}
	id, _ := r.LastInsertId()
	return id, tok, nil
}
