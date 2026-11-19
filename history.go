package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

var (
	recordIDPattern = regexp.MustCompile(`^[a-z2-7]{26,64}$`)
	commitPattern   = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)
	sha256Pattern   = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type deployment struct {
	ID           string        `json:"id"`
	Kind         string        `json:"kind"`
	Status       string        `json:"status"`
	Message      string        `json:"message,omitempty"`
	Name         string        `json:"name"`
	OriginalName string        `json:"originalName"`
	Object       string        `json:"object"`
	SHA256       string        `json:"sha256"`
	Archived     bool          `json:"archived"`
	Size         int64         `json:"size"`
	Run          string        `json:"run"`
	Commit       string        `json:"commit,omitempty"`
	Operator     administrator `json:"operator"`
	Target       string        `json:"target,omitempty"`
	Started      time.Time     `json:"started"`
	Finished     *time.Time    `json:"finished,omitempty"`
}

type history string

func openHistory(values map[string]string) (history, error) {
	dir := values["DEPLOY_STATE_DIR"]
	if dir == "" {
		dir = os.Getenv("STATE_DIRECTORY")
	}
	if dir == "" {
		dir = "data"
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	store := history(dir)
	records, err := store.list()
	if err != nil {
		return "", err
	}
	for _, record := range records {
		if record.Status == "running" {
			if err := store.finish(record, "interrupted", "Deplyt 在完成确认前重启，请核实生产版本"); err != nil {
				return "", err
			}
		}
	}
	return store, nil
}

func (h history) save(record deployment) error {
	f, err := os.CreateTemp(string(h), ".deployment-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := json.NewEncoder(f).Encode(record); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(string(h), record.ID+".json")); err != nil {
		return err
	}
	dir, err := os.Open(string(h))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (h history) load(id string) (deployment, error) {
	if !recordIDPattern.MatchString(id) {
		return deployment{}, os.ErrNotExist
	}
	data, err := os.ReadFile(filepath.Join(string(h), id+".json"))
	if err != nil {
		return deployment{}, err
	}
	var record deployment
	if err := json.Unmarshal(data, &record); err != nil {
		return deployment{}, err
	}
	if record.ID != id || !jarNamePattern.MatchString(record.Name) ||
		!sha256Pattern.MatchString(record.SHA256) || record.Object != archivePrefix+record.Name {
		return deployment{}, fmt.Errorf("invalid deployment record %s", id)
	}
	return record, nil
}

func (h history) list() ([]deployment, error) {
	files, err := os.ReadDir(string(h))
	if err != nil {
		return nil, err
	}
	records := make([]deployment, 0, len(files))
	for _, file := range files {
		id, ok := strings.CutSuffix(file.Name(), ".json")
		if file.IsDir() || !ok || !recordIDPattern.MatchString(id) {
			continue
		}
		record, err := h.load(id)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	slices.SortFunc(records, func(a, b deployment) int { return b.Started.Compare(a.Started) })
	return records, nil
}

func (h history) finish(record deployment, status, message string) error {
	now := time.Now().UTC()
	record.Status, record.Message, record.Finished = status, message, &now
	return h.save(record)
}

func (s *relay) handleHistory(w http.ResponseWriter, _ *http.Request, _ administrator) {
	records, err := s.history.list()
	if err != nil {
		s.logger.Error("load deployment history", "error", err)
		http.Error(w, "无法读取部署记录", http.StatusInternalServerError)
		return
	}
	writeJSON(w, records)
}

func (s *relay) handleRollback(w http.ResponseWriter, r *http.Request, admin administrator) {
	if !s.mu.TryLock() {
		http.Error(w, "已有部署或回滚正在进行", http.StatusConflict)
		return
	}
	defer s.mu.Unlock()

	target, err := s.history.load(r.PathValue("id"))
	if errors.Is(err, os.ErrNotExist) || (err == nil && (!target.Archived || target.Status != "succeeded")) {
		http.Error(w, "找不到已验证成功的 OSS 版本", http.StatusNotFound)
		return
	}
	if err != nil {
		s.logger.Error("load rollback target", "error", err)
		http.Error(w, "无法读取回滚版本", http.StatusInternalServerError)
		return
	}
	if r.Context().Err() != nil {
		return
	}

	record := target
	record.ID, record.Kind, record.Status = strings.ToLower(rand.Text()), "rollback", "running"
	record.Operator, record.Target = admin, target.ID
	record.Started, record.Finished = time.Now().UTC(), nil
	if err := s.history.save(record); err != nil {
		s.logger.Error("create rollback record", "error", err)
		http.Error(w, "无法保存回滚记录", http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), deployTimeout)
	defer cancel()
	if !s.apply(w, ctx, record) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *relay) apply(w http.ResponseWriter, ctx context.Context, record deployment) bool {
	logger := s.logger.With("deployment_id", record.ID, "run_id", record.Run, "object", record.Object)
	err := s.activate(ctx, artifact{name: record.Name, key: record.Object, sha256: record.SHA256}, record.ID, logger)
	status, message := "succeeded", ""
	if err != nil {
		logger.Error("production deployment failed", "error", err)
		status, message = "failed", "生产切换未获确认，请查看 Deplyt 日志"
	}
	if err := s.history.finish(record, status, message); err != nil {
		logger.Error("save deployment result", "error", err)
		http.Error(w, "无法保存部署结果，请查看 Deplyt 日志", http.StatusInternalServerError)
		return false
	}
	if status == "failed" {
		http.Error(w, message, http.StatusBadGateway)
		return false
	}
	logger.Info("production deployment succeeded")
	return true
}
