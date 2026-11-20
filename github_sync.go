package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const githubSyncInterval = 3 * time.Hour

var githubFeedOrder = []string{"commits", "pulls", "actions", "dependabot", "code", "secrets"}

type githubSnapshot struct {
	Items     []json.RawMessage             `json:"items"`
	Details   map[string]githubCachedDetail `json:"details,omitempty"`
	UpdatedAt time.Time                     `json:"updatedAt"`
}

type githubSyncResult struct {
	StartedAt  time.Time         `json:"startedAt,omitzero"`
	FinishedAt time.Time         `json:"finishedAt,omitzero"`
	Issues     map[string]string `json:"issues,omitempty"`
}

type githubSyncStatus struct {
	githubSyncResult
	Enabled  bool      `json:"enabled"`
	Running  bool      `json:"running"`
	NextAt   time.Time `json:"nextAt,omitzero"`
	Revision uint64    `json:"revision"`
}

type githubSync struct {
	repo   githubRepository
	dir    string
	logger *slog.Logger
	wake   chan struct{}

	mu        sync.RWMutex
	snapshots map[string]githubSnapshot
	result    githubSyncResult
	running   bool
	revision  uint64
}

func openGitHubSync(repo githubRepository, stateDir string, logger *slog.Logger) *githubSync {
	s := &githubSync{
		repo: repo, dir: filepath.Join(stateDir, "github"), logger: logger,
		wake: make(chan struct{}, 1), snapshots: make(map[string]githubSnapshot),
	}
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		logger.Warn("create GitHub cache directory", "error", err)
	}
	for _, name := range githubFeedOrder {
		var snapshot githubSnapshot
		if s.read(name+".json", &snapshot) && !snapshot.UpdatedAt.IsZero() {
			s.snapshots[name] = snapshot
		}
	}
	var result githubSyncResult
	if s.read("sync.json", &result) {
		s.result = result
	}
	for _, snapshot := range s.snapshots {
		if snapshot.UpdatedAt.After(s.result.FinishedAt) {
			s.result.StartedAt = time.Time{}
			break
		}
	}
	return s
}

func (s *githubSync) read(name string, output any) bool {
	file, err := os.Open(filepath.Join(s.dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err == nil {
		defer file.Close()
		err = json.NewDecoder(file).Decode(output)
	}
	if err != nil {
		s.logger.Warn("read GitHub cache", "file", name, "error", err)
	}
	return err == nil
}

func (s *githubSync) save(name string, output any) {
	if err := writeStateJSON(filepath.Join(s.dir, name), output); err != nil {
		s.logger.Warn("save GitHub cache", "file", name, "error", err)
	}
}

func (s *githubSync) status() githubSyncStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	status := githubSyncStatus{
		githubSyncResult: s.result, Enabled: s.repo.token != "", Running: s.running, Revision: s.revision,
	}
	if status.Enabled && !s.result.StartedAt.IsZero() {
		status.NextAt = s.result.StartedAt.Add(githubSyncInterval)
	}
	return status
}

func (s *githubSync) request() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		s.running = true
		s.wake <- struct{}{}
	}
}

func (s *githubSync) run(ctx context.Context) {
	if s.repo.token == "" {
		return
	}
	pull := githubPull{repo: s.repo, pace: rate.NewLimiter(rate.Every(200*time.Millisecond), 1)}
	for ctx.Err() == nil {
		delay := time.Until(s.status().NextAt)
		timer := time.NewTimer(max(0, delay))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			s.request()
			<-s.wake
		case <-s.wake:
		}
		timer.Stop()
		s.refresh(ctx, pull)
	}
}

func (s *githubSync) snapshot(name string) githubSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshots[name]
}

func (s *githubSync) publish(name string, snapshot githubSnapshot) {
	s.save(name+".json", snapshot)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshots[name] = snapshot
	s.revision++
}

func (s *githubSync) refresh(ctx context.Context, pull githubPull) {
	started := time.Now().UTC()
	issues := make(map[string]string)
	fresh := make(map[string]githubSnapshot)
	var stopped error
	for _, name := range githubFeedOrder {
		if ctx.Err() != nil {
			break
		}
		if stopped != nil {
			issues[name] = stopped.Error()
			continue
		}
		items, err := pull.list(ctx, name)
		if err != nil {
			issues[name] = err.Error()
			if stopGitHubPull(err) {
				stopped = err
			}
			continue
		}
		snapshot := s.snapshot(name)
		snapshot.Items, snapshot.UpdatedAt = items, time.Now().UTC()
		fresh[name] = snapshot
		s.publish(name, snapshot)
	}
	for _, name := range []string{"commits", "pulls", "actions"} {
		snapshot, ok := fresh[name]
		if !ok {
			continue
		}
		details, missed, stop := syncGitHubDetails(ctx, pull, name, snapshot, stopped)
		if missed > 0 {
			issues[name] = fmt.Sprintf("%d 条详情暂未同步", missed)
			if stop != nil {
				issues[name] += "，" + stop.Error()
			}
		}
		stopped = stop
		snapshot.Details = details
		s.publish(name, snapshot)
	}
	if ctx.Err() != nil {
		return
	}
	for name, issue := range issues {
		s.logger.Warn("GitHub 同步未完成", "section", name, "message", issue)
	}
	result := githubSyncResult{StartedAt: started, FinishedAt: time.Now().UTC(), Issues: issues}
	s.save("sync.json", result)
	s.mu.Lock()
	s.result, s.running = result, false
	s.mu.Unlock()
}

func syncGitHubDetails(ctx context.Context, pull githubPull, name string, snapshot githubSnapshot, stopped error) (map[string]githubCachedDetail, int, error) {
	details := make(map[string]githubCachedDetail, len(snapshot.Items))
	missed := 0
	for _, item := range snapshot.Items {
		id, revision, reuse := githubDetailID(name, item)
		if id == "" {
			missed++
			continue
		}
		previous, ok := snapshot.Details[id]
		if ok {
			details[id] = previous
			if reuse && previous.Revision == revision {
				continue
			}
		}
		if stopped == nil && ctx.Err() == nil {
			data, err := pull.detail(ctx, name, id)
			if err == nil {
				details[id] = githubCachedDetail{Revision: revision, Data: data}
				continue
			}
			if stopGitHubPull(err) {
				stopped = err
			}
		}
		missed++
	}
	return details, missed, stopped
}

func (s *githubSync) list(w http.ResponseWriter, r *http.Request, _ administrator) {
	name := r.PathValue("section")
	if _, ok := githubFeeds[name]; !ok {
		http.NotFound(w, r)
		return
	}
	page := 1
	if value := r.URL.Query().Get("page"); value != "" {
		var err error
		page, err = strconv.Atoi(value)
		if err != nil || page < 1 {
			http.Error(w, "页码无效", http.StatusBadRequest)
			return
		}
	}
	const size = 20
	snapshot := s.snapshot(name)
	items := []json.RawMessage{}
	details := make(map[string]json.RawMessage)
	hasNext := false
	if page-1 <= len(snapshot.Items)/size {
		start := (page - 1) * size
		end := min(start+size, len(snapshot.Items))
		items = append(items, snapshot.Items[start:end]...)
		hasNext = end < len(snapshot.Items)
	}
	if len(snapshot.Details) > 0 {
		for _, item := range items {
			id, _, _ := githubDetailID(name, item)
			if detail, ok := snapshot.Details[id]; ok {
				details[id] = detail.Data
			}
		}
	}
	writeJSON(w, struct {
		Items     []json.RawMessage          `json:"items"`
		Details   map[string]json.RawMessage `json:"details"`
		HasNext   bool                       `json:"hasNext"`
		UpdatedAt time.Time                  `json:"updatedAt,omitzero"`
	}{items, details, hasNext, snapshot.UpdatedAt})
}

func (s *githubSync) detail(w http.ResponseWriter, r *http.Request, _ administrator) {
	name, id := r.PathValue("section"), r.PathValue("id")
	if !((name == "commits" && commitPattern.MatchString(id)) ||
		((name == "actions" || name == "pulls") && runIDPattern.MatchString(id))) {
		http.NotFound(w, r)
		return
	}
	snapshot := s.snapshot(name)
	detail, ok := snapshot.Details[id]
	if !ok {
		http.Error(w, "这条记录的详情尚未同步", http.StatusNotFound)
		return
	}
	writeJSON(w, detail.Data)
}

func (s *githubSync) syncStatus(w http.ResponseWriter, _ *http.Request, _ administrator) {
	writeJSON(w, s.status())
}

func (s *githubSync) syncNow(w http.ResponseWriter, _ *http.Request, admin administrator) {
	if s.repo.token == "" {
		http.Error(w, "Deplyt 尚未配置 GITHUB_TOKEN", http.StatusServiceUnavailable)
		return
	}
	s.request()
	s.logger.Info("requested full GitHub synchronization", "user_id", admin.ID)
	writeJSON(w, s.status())
}
