package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"
)

// A backup set runs several plans together (for example one per shard) and remembers which
// backups belong to the same run, so a whole platform can be restored to one moment.
type PlanSet struct {
	ID        int64   `json:"id"`
	Name      string  `json:"name"`
	PlanIDs   []int64 `json:"planIds"`
	Schedule  string  `json:"schedule"`
	NextRunAt int64   `json:"nextRunAt"`
}

func (a *App) Sets() ([]*PlanSet, error) {
	rows, err := a.Store.DB.Query(`SELECT id,name,plan_ids,schedule,next_run_at FROM plan_sets ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*PlanSet{}
	for rows.Next() {
		var s PlanSet
		var ids string
		if err := rows.Scan(&s.ID, &s.Name, &ids, &s.Schedule, &s.NextRunAt); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(ids), &s.PlanIDs)
		out = append(out, &s)
	}
	return out, rows.Err()
}

func (a *App) Set(id int64) (*PlanSet, error) {
	all, err := a.Sets()
	if err != nil {
		return nil, err
	}
	for _, s := range all {
		if s.ID == id {
			return s, nil
		}
	}
	return nil, errors.New("backup set not found")
}

func (a *App) SaveSet(s *PlanSet) error {
	if s.Name == "" || len(s.PlanIDs) < 2 {
		return errors.New("a set needs a name and at least two plans")
	}
	for _, id := range s.PlanIDs {
		p, err := a.Store.Plan(id)
		if err != nil {
			return fmt.Errorf("plan %d does not exist", id)
		}
		if p.Spec.Method == "binlog" {
			return fmt.Errorf("plan %q archives binary logs and cannot be part of a set", p.Name)
		}
	}
	next, err := NextRun(s.Schedule, time.Now())
	if err != nil {
		return err
	}
	ids, _ := json.Marshal(s.PlanIDs)
	if s.ID == 0 {
		r, err := a.Store.DB.Exec(`INSERT INTO plan_sets(name,plan_ids,schedule,next_run_at,created_at) VALUES(?,?,?,?,?)`, s.Name, string(ids), s.Schedule, next, now())
		if err != nil {
			return err
		}
		s.ID, _ = r.LastInsertId()
		return nil
	}
	_, err = a.Store.DB.Exec(`UPDATE plan_sets SET name=?,plan_ids=?,schedule=?,next_run_at=? WHERE id=?`, s.Name, string(ids), s.Schedule, next, s.ID)
	return err
}

// RunSet starts every plan of the set under one run id. Plans on different servers run at the same
// time; the queue still keeps one job per server.
func (a *App) RunSet(s *PlanSet, trigger string) (string, error) {
	run := fmt.Sprintf("set%d-%s", s.ID, time.Now().UTC().Format("20060102T150405Z"))
	started := 0
	var firstErr error
	for _, id := range s.PlanIDs {
		p, err := a.Store.Plan(id)
		if err == nil && a.Queue.ActiveForPlan(id) {
			err = fmt.Errorf("plan %q is already running", p.Name)
		}
		if err == nil {
			_, err = a.StartBackupIn(p, trigger, run)
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		started++
	}
	if started == 0 {
		return "", firstErr
	}
	if firstErr != nil {
		log.Printf("backup set %q: started %d of %d plans: %v", s.Name, started, len(s.PlanIDs), firstErr)
	}
	return run, nil
}

type SetRun struct {
	Run      string    `json:"run"`
	Backups  []*Backup `json:"backups"`
	Complete bool      `json:"complete"` // every plan of the set has a finished backup in this run
	// CommonFrom is the earliest moment all members can be brought to together: each one is rolled
	// forward with its binary logs from its own backup moment to this time or any later one.
	CommonFrom int64 `json:"commonFrom"`
}

func (a *App) SetRuns(s *PlanSet, limit int) ([]SetRun, error) {
	rows, err := a.Store.DB.Query(`SELECT `+backupCols+` FROM backups WHERE set_run LIKE ? ORDER BY created_at DESC, id`, fmt.Sprintf("set%d-%%", s.ID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SetRun
	idx := map[string]int{}
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		i, ok := idx[b.SetRun]
		if !ok {
			if len(out) >= limit {
				continue
			}
			i = len(out)
			idx[b.SetRun] = i
			out = append(out, SetRun{Run: b.SetRun})
		}
		out[i].Backups = append(out[i].Backups, b)
	}
	for i := range out {
		good := map[int64]bool{}
		for _, b := range out[i].Backups {
			if b.Status == "done" && b.DeletedAt == 0 {
				good[b.PlanID] = true
				if b.FinishedAt > out[i].CommonFrom {
					out[i].CommonFrom = b.FinishedAt
				}
			}
		}
		out[i].Complete = len(good) == len(s.PlanIDs)
		if !out[i].Complete {
			out[i].CommonFrom = 0
		}
	}
	return out, rows.Err()
}

func (a *App) setsTick() {
	sets, err := a.Sets()
	if err != nil {
		return
	}
	for _, s := range sets {
		if s.NextRunAt == 0 || s.NextRunAt > now() {
			continue
		}
		next, _ := NextRun(s.Schedule, time.Now())
		a.Store.DB.Exec(`UPDATE plan_sets SET next_run_at=? WHERE id=?`, next, s.ID)
		if _, err := a.RunSet(s, "schedule"); err != nil {
			log.Printf("backup set %q: %v", s.Name, err)
			a.Notify("failure", fmt.Sprintf("Backup set could not start: %s: %v", s.Name, err))
		}
	}
}
