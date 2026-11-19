package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"backupmanager/internal/core"
)

// agentCmd runs this program as an agent: it only makes outgoing calls to the panel, asks for
// work, runs the backup next to the data and streams it to the destination itself.
func agentCmd(args []string) error {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	server := fs.String("server", env("BM_AGENT_SERVER", os.Getenv("BM_SERVER")), "address of the panel, e.g. https://backups.example.com")
	token := fs.String("token", os.Getenv("BM_AGENT_TOKEN"), "this agent's token (or BM_AGENT_TOKEN)")
	insecure := fs.Bool("allow-http", false, "allow a plain http:// panel address (passwords travel on this link)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	u, err := url.Parse(strings.TrimRight(*server, "/"))
	if err != nil || u.Host == "" || *token == "" {
		return errors.New("usage: backup-manager agent --server https://panel.example.com --token bma_…")
	}
	local := u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost"
	if u.Scheme != "https" && !local && !*insecure {
		return errors.New("the panel address must be https:// (database passwords travel on this link). For a private network add --allow-http")
	}
	dir := filepath.Join(env("BM_DATA_DIR", "/var/lib/backup-manager"), "agent")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	os.Remove(filepath.Join(dir, "catalog.db")) // the agent keeps nothing between runs
	keys, _, err := core.LoadOrCreateMaster(filepath.Join(dir, "agent.key"))
	if err != nil {
		return err
	}
	st, err := core.OpenStore(filepath.Join(dir, "catalog.db"))
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	post := func(ctx context.Context, path string, body any, timeout time.Duration) (*http.Response, error) {
		b, _ := json.Marshal(body)
		c, cancel := context.WithTimeout(ctx, timeout)
		req, _ := http.NewRequestWithContext(c, "POST", u.String()+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+*token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-BM-Host", host)
		req.Header.Set("X-BM-Version", version)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			return nil, err
		}
		go func() { <-c.Done(); cancel() }()
		return resp, nil
	}
	log.Printf("backup-manager agent %s on %s, serving %s", version, host, u.String())
	wait := time.Second
	for ctx.Err() == nil {
		resp, err := post(ctx, "/api/agent/poll", nil, 40*time.Second)
		if err != nil || (resp.StatusCode != 200 && resp.StatusCode != 204) {
			if err == nil {
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
				resp.Body.Close()
				err = fmt.Errorf("%s %s", resp.Status, strings.TrimSpace(string(b)))
			}
			if ctx.Err() == nil {
				log.Printf("cannot reach the panel: %v (trying again in %s)", err, wait)
			}
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
			if wait < 30*time.Second {
				wait *= 2
			}
			continue
		}
		wait = time.Second
		if resp.StatusCode == 204 {
			resp.Body.Close()
			continue
		}
		var task core.AgentTask
		err = json.NewDecoder(resp.Body).Decode(&task)
		resp.Body.Close()
		if err != nil {
			log.Printf("unreadable task: %v", err)
			continue
		}
		log.Printf("task %s: %s", task.ID, task.Plan.Name)
		app := &core.App{Store: st, Keys: keys, Hub: core.NewHub()}
		tctx, cancel := context.WithCancel(ctx)
		var mu sync.Mutex
		var latest any
		done := make(chan struct{})
		go func() { // progress goes up every 2 seconds; the answer says whether to stop
			t := time.NewTicker(2 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case <-t.C:
					mu.Lock()
					p := latest
					mu.Unlock()
					resp, err := post(tctx, "/api/agent/progress", map[string]any{"taskId": task.ID, "progress": p}, 15*time.Second)
					if err != nil {
						continue
					}
					var out struct{ Cancel bool }
					json.NewDecoder(resp.Body).Decode(&out)
					resp.Body.Close()
					if out.Cancel {
						log.Printf("task %s: the panel asked to stop", task.ID)
						cancel()
					}
				}
			}
		}()
		res := app.RunAgentTask(tctx, &task, func(p any) { mu.Lock(); latest = p; mu.Unlock() })
		close(done)
		cancel()
		if res.Error != "" {
			log.Printf("task %s failed: %s", task.ID, res.Error)
		} else {
			log.Printf("task %s done", task.ID)
		}
		for i := 0; i < 5; i++ { // the result must reach the panel; retry a few times
			if resp, err := post(context.Background(), "/api/agent/result", res, 30*time.Second); err == nil {
				resp.Body.Close()
				break
			}
			time.Sleep(3 * time.Second)
		}
	}
	return nil
}
