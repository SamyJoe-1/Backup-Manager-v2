package core

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// ParseSchedule accepts "" (manual only), "@every 6h", "@daily" or a 5-field cron line.
// "CRON_TZ=Africa/Cairo 0 3 * * *" sets the time zone of a cron line.
func ParseSchedule(s string) (cron.Schedule, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	sch, err := cron.ParseStandard(s)
	if err != nil {
		return nil, fmt.Errorf("schedule %q is not valid: %w", s, err)
	}
	if next := sch.Next(time.Now()); sch.Next(next).Sub(next) < 5*time.Minute {
		return nil, fmt.Errorf("schedule %q runs more often than every 5 minutes", s)
	}
	return sch, nil
}

// NextRun returns the next start time (unix) or 0 for manual plans.
func NextRun(schedule string, from time.Time) (int64, error) {
	sch, err := ParseSchedule(schedule)
	if err != nil || sch == nil {
		return 0, err
	}
	return sch.Next(from).Unix(), nil
}

// RunScheduler starts due plans. A plan that missed several runs (manager was off) starts once.
func (a *App) RunScheduler(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	lastSweep, lastSandbox := time.Time{}, time.Time{}
	for {
		a.schedTick()
		a.setsTick()
		if time.Since(lastSandbox) > time.Minute {
			lastSandbox = time.Now()
			a.SweepSandboxes()
		}
		if time.Since(lastSweep) > 6*time.Hour {
			lastSweep = time.Now()
			a.sweep(ctx)
			if a.CatalogCopy != "" { // a consistent copy that a folder plan can store off the server
				if err := SnapshotCatalog(a.Store, a.CatalogCopy); err != nil {
					log.Printf("catalog copy: %v", err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (a *App) schedTick() {
	plans, err := a.Store.Plans(`enabled=1 AND next_run_at>0 AND next_run_at<=?`, now())
	if err != nil {
		log.Printf("scheduler: %v", err)
		return
	}
	for _, p := range plans {
		next, err := NextRun(p.Schedule, time.Now())
		if err != nil {
			log.Printf("scheduler: plan %q: %v", p.Name, err)
		}
		a.Store.DB.Exec(`UPDATE plans SET next_run_at=? WHERE id=?`, next, p.ID)
		if a.Queue.ActiveForPlan(p.ID) {
			log.Printf("scheduler: plan %q is still running, this run is skipped", p.Name)
			continue
		}
		if _, err := a.StartBackup(p, "schedule"); err != nil {
			log.Printf("scheduler: plan %q: %v", p.Name, err)
			a.Notify("failure", fmt.Sprintf("Backup could not start: %s: %v", p.Name, err))
		}
	}
}

// sweep applies retention for every plan and trims old history rows.
func (a *App) sweep(ctx context.Context) {
	plans, _ := a.Store.Plans("")
	for _, p := range plans {
		if _, err := a.ApplyRetention(ctx, p); err != nil {
			log.Printf("retention for plan %q: %v", p.Name, err)
		}
	}
	old := time.Now().AddDate(0, 0, -90).Unix()
	a.Store.DB.Exec(`DELETE FROM jobs WHERE state IN ('done','failed','cancelled') AND finished_at<?`, old)
	a.Store.DB.Exec(`DELETE FROM backups WHERE (deleted_at>0 AND deleted_at<?) OR (status IN ('failed','cancelled') AND created_at<?)`, old, old)
	a.Store.DB.Exec(`DELETE FROM sessions WHERE expires_at<?`, now())
}
