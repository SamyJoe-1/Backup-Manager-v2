package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"time"
)

// Job is one unit of long work kept in the catalog, so it survives a restart.
type Job struct {
	ID          int64           `json:"id"`
	Kind        string          `json:"kind"`
	Payload     json.RawMessage `json:"-"`
	State       string          `json:"state"` // queued | running | done | failed | cancelled
	TargetID    int64           `json:"targetId"`
	PlanID      int64           `json:"planId"`
	BackupID    string          `json:"backupId"`
	Title       string          `json:"title"`
	Priority    int             `json:"priority"`
	Attempt     int             `json:"attempt"`
	MaxAttempts int             `json:"maxAttempts"`
	RunAfter    int64           `json:"runAfter"`
	Progress    json.RawMessage `json:"progress"`
	Error       string          `json:"error"`
	CreatedAt   int64           `json:"createdAt"`
	StartedAt   int64           `json:"startedAt"`
	FinishedAt  int64           `json:"finishedAt"`
}

const jobCols = `id,kind,payload,state,target_id,plan_id,backup_id,title,priority,attempt,max_attempts,run_after,progress,error,created_at,started_at,finished_at`

func scanJob(r scanner) (*Job, error) {
	var j Job
	var payload, progress string
	err := r.Scan(&j.ID, &j.Kind, &payload, &j.State, &j.TargetID, &j.PlanID, &j.BackupID, &j.Title, &j.Priority, &j.Attempt, &j.MaxAttempts, &j.RunAfter, &progress, &j.Error, &j.CreatedAt, &j.StartedAt, &j.FinishedAt)
	j.Payload, j.Progress = json.RawMessage(payload), json.RawMessage(progress)
	return &j, err
}

// Handler runs a job. report stores and publishes progress. Returning ErrNoRetry-wrapped errors
// fails the job at once; other errors are retried until the attempts run out.
type Handler func(ctx context.Context, j *Job, report func(any)) error

var ErrNoRetry = errors.New("not retryable")

// Hub fans job events out to the panel (server-sent events).
type Hub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

func NewHub() *Hub { return &Hub{subs: map[chan []byte]struct{}{}} }

func (h *Hub) Subscribe() (chan []byte, func()) {
	ch := make(chan []byte, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() { h.mu.Lock(); delete(h.subs, ch); h.mu.Unlock() }
}

func (h *Hub) Publish(event string, data any) {
	b, _ := json.Marshal(map[string]any{"event": event, "data": data})
	h.mu.Lock()
	for ch := range h.subs {
		select {
		case ch <- b:
		default: // a slow browser loses an update, never blocks a backup
		}
	}
	h.mu.Unlock()
}

type Queue struct {
	st        *Store
	hub       *Hub
	handlers  map[string]Handler
	workers   int
	wake      chan struct{}
	mu        sync.Mutex
	cancels   map[int64]context.CancelFunc
	RetryBase time.Duration
}

func NewQueue(st *Store, hub *Hub, workers int) *Queue {
	base := 30 * time.Second
	if s, err := strconv.Atoi(os.Getenv("BM_RETRY_BASE_SECONDS")); err == nil && s > 0 {
		base = time.Duration(s) * time.Second
	}
	return &Queue{st: st, hub: hub, handlers: map[string]Handler{}, workers: workers, wake: make(chan struct{}, 1), cancels: map[int64]context.CancelFunc{}, RetryBase: base}
}

func (q *Queue) Handle(kind string, h Handler) { q.handlers[kind] = h }

func (q *Queue) Enqueue(j *Job) error {
	if j.MaxAttempts == 0 {
		j.MaxAttempts = 3
	}
	if len(j.Payload) == 0 {
		j.Payload = json.RawMessage(`{}`)
	}
	r, err := q.st.DB.Exec(`INSERT INTO jobs(kind,payload,state,target_id,plan_id,backup_id,title,priority,max_attempts,run_after,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		j.Kind, string(j.Payload), "queued", j.TargetID, j.PlanID, j.BackupID, j.Title, j.Priority, j.MaxAttempts, j.RunAfter, now())
	if err != nil {
		return err
	}
	j.ID, _ = r.LastInsertId()
	j.State = "queued"
	q.publish(j.ID)
	q.Wake()
	return nil
}

func (q *Queue) Wake() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *Queue) Job(id int64) (*Job, error) {
	return scanJob(q.st.DB.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id=?`, id))
}

func (q *Queue) List(limit, offset int, state string) ([]*Job, int, error) {
	where, args := "", []any{}
	if state != "" {
		where, args = " WHERE state=?", []any{state}
	}
	var total int
	if err := q.st.DB.QueryRow(`SELECT COUNT(*) FROM jobs`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := q.st.DB.Query(`SELECT `+jobCols+` FROM jobs`+where+` ORDER BY id DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, j)
	}
	return out, total, rows.Err()
}

func (q *Queue) publish(id int64) {
	if j, err := q.Job(id); err == nil {
		q.hub.Publish("job", j)
	}
}

// Hold stops new jobs from starting (used around deploys and migrations). Running jobs finish.
func (q *Queue) Hold(on bool) {
	v := ""
	if on {
		v = "1"
	}
	q.st.SetSetting("hold", v)
	q.hub.Publish("hold", on)
	q.Wake()
}

// Held reads the flag from the catalog so `backup-manager hold` from a shell reaches a running server.
func (q *Queue) Held() bool { return q.st.Setting("hold") == "1" }

// Cancel stops a running job or drops a queued one.
func (q *Queue) Cancel(id int64) error {
	q.mu.Lock()
	cancel := q.cancels[id]
	q.mu.Unlock()
	if cancel != nil {
		q.st.DB.Exec(`UPDATE jobs SET cancel=1 WHERE id=?`, id)
		cancel()
		return nil
	}
	r, err := q.st.DB.Exec(`UPDATE jobs SET state='cancelled', finished_at=?, error='cancelled before it started' WHERE id=? AND state='queued'`, now(), id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return fmt.Errorf("job %d is not waiting or running", id)
	}
	if j, err := q.Job(id); err == nil && j.BackupID != "" {
		q.st.DB.Exec(`UPDATE backups SET status='cancelled', finished_at=?, error='cancelled' WHERE id=? AND status IN ('queued','running')`, now(), j.BackupID)
	}
	q.publish(id)
	return nil
}

// Recover runs once at start: jobs that were running when the process died go back to the queue
// (or fail when their attempts are used up).
func (q *Queue) Recover() {
	rows, err := q.st.DB.Query(`SELECT id, attempt, max_attempts, backup_id FROM jobs WHERE state='running'`)
	if err != nil {
		return
	}
	type rec struct {
		id       int64
		att, max int
		backup   string
	}
	var list []rec
	for rows.Next() {
		var r rec
		rows.Scan(&r.id, &r.att, &r.max, &r.backup)
		list = append(list, r)
	}
	rows.Close()
	for _, r := range list {
		if r.att < r.max {
			q.st.DB.Exec(`UPDATE jobs SET state='queued', run_after=?, error='interrupted by a restart; running again' WHERE id=?`, now(), r.id)
			q.st.DB.Exec(`UPDATE backups SET status='queued' WHERE id=? AND status='running'`, r.backup)
			log.Printf("job %d was interrupted by a restart: queued again (attempt %d of %d used)", r.id, r.att, r.max)
		} else {
			q.st.DB.Exec(`UPDATE jobs SET state='failed', finished_at=?, error='interrupted by a restart; no attempts left' WHERE id=?`, now(), r.id)
			q.st.DB.Exec(`UPDATE backups SET status='failed', finished_at=?, error='interrupted by a restart' WHERE id=? AND status='running'`, now(), r.backup)
		}
	}
}

func (q *Queue) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < q.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(2 * time.Second)
			defer t.Stop()
			for {
				for q.runOne(ctx) {
				}
				select {
				case <-ctx.Done():
					return
				case <-q.wake:
				case <-t.C:
				}
			}
		}()
	}
	wg.Wait()
}

// claim takes the next job. A target never has two jobs running at once.
func (q *Queue) claim() *Job {
	if q.Held() {
		return nil
	}
	tx, err := q.st.DB.Begin()
	if err != nil {
		return nil
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRow(`SELECT id FROM jobs WHERE state='queued' AND run_after<=? AND (target_id=0 OR target_id NOT IN (SELECT target_id FROM jobs WHERE state='running'))
		ORDER BY priority DESC, id LIMIT 1`, now()).Scan(&id)
	if err != nil {
		return nil
	}
	if _, err := tx.Exec(`UPDATE jobs SET state='running', attempt=attempt+1, started_at=?, heartbeat_at=?, error='', cancel=0, progress='{}' WHERE id=?`, now(), now(), id); err != nil {
		return nil
	}
	j, err := scanJob(tx.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id=?`, id))
	if err != nil || tx.Commit() != nil {
		return nil
	}
	return j
}

func (q *Queue) runOne(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	j := q.claim()
	if j == nil {
		return false
	}
	q.hub.Publish("job", j)
	jctx, cancel := context.WithCancel(ctx)
	q.mu.Lock()
	q.cancels[j.ID] = cancel
	q.mu.Unlock()

	stop := make(chan struct{})
	go func() { // heartbeat
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				q.st.DB.Exec(`UPDATE jobs SET heartbeat_at=? WHERE id=?`, now(), j.ID)
			}
		}
	}()

	var last time.Time
	report := func(p any) {
		if time.Since(last) < 700*time.Millisecond {
			return
		}
		last = time.Now()
		b, _ := json.Marshal(p)
		q.st.DB.Exec(`UPDATE jobs SET progress=? WHERE id=?`, string(b), j.ID)
		q.hub.Publish("progress", map[string]any{"id": j.ID, "backupId": j.BackupID, "progress": json.RawMessage(b)})
	}

	err := q.safeRun(jctx, j, report)
	close(stop)
	cancel()
	q.mu.Lock()
	delete(q.cancels, j.ID)
	q.mu.Unlock()

	var cancelled int
	q.st.DB.QueryRow(`SELECT cancel FROM jobs WHERE id=?`, j.ID).Scan(&cancelled)
	switch {
	case err == nil:
		q.st.DB.Exec(`UPDATE jobs SET state='done', finished_at=?, error='' WHERE id=?`, now(), j.ID)
	case cancelled == 1:
		q.st.DB.Exec(`UPDATE jobs SET state='cancelled', finished_at=?, error='cancelled' WHERE id=?`, now(), j.ID)
	case ctx.Err() != nil:
		// The manager is stopping: leave the job as running; Recover() queues it again at next start.
		return false
	case j.Attempt < j.MaxAttempts && !errors.Is(err, ErrNoRetry):
		delay := q.RetryBase << (j.Attempt - 1)
		if delay > 15*time.Minute {
			delay = 15 * time.Minute
		}
		q.st.DB.Exec(`UPDATE jobs SET state='queued', run_after=?, error=? WHERE id=?`, time.Now().Add(delay).Unix(), "will retry: "+err.Error(), j.ID)
		log.Printf("job %d (%s) failed, retry in %s: %v", j.ID, j.Title, delay, err)
	default:
		q.st.DB.Exec(`UPDATE jobs SET state='failed', finished_at=?, error=? WHERE id=?`, now(), err.Error(), j.ID)
		log.Printf("job %d (%s) failed: %v", j.ID, j.Title, err)
	}
	q.publish(j.ID)
	return true
}

func (q *Queue) safeRun(ctx context.Context, j *Job, report func(any)) (err error) {
	h := q.handlers[j.Kind]
	if h == nil {
		return fmt.Errorf("no handler for job kind %q: %w", j.Kind, ErrNoRetry)
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("internal error: %v", r)
		}
	}()
	return h(ctx, j, report)
}

// ActiveForPlan tells whether a plan already has a job waiting or running.
func (q *Queue) ActiveForPlan(planID int64) bool {
	var n int
	q.st.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE plan_id=? AND state IN ('queued','running')`, planID).Scan(&n)
	return n > 0
}
