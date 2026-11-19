package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

type accessAccount struct {
	ID          int64
	Username    string
	DisplayName string
	Granted     bool
}

type accessRegistry struct {
	mu     sync.Mutex
	admin  int64
	path   string
	issuer string
	users  map[int64]accessAccount
}

type accessState struct {
	Issuer string                  `json:"issuer"`
	Users  map[int64]accessAccount `json:"users"`
}

func openAccessRegistry(dir, issuer string, admin int64) (*accessRegistry, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	registry := &accessRegistry{
		admin: admin, issuer: issuer, path: filepath.Join(dir, "access.json"),
		users: make(map[int64]accessAccount),
	}
	data, err := os.ReadFile(registry.path)
	if errors.Is(err, os.ErrNotExist) {
		return registry, nil
	}
	if err != nil {
		return nil, err
	}
	var state accessState
	if err := json.Unmarshal(data, &state); err != nil || state.Issuer != issuer || state.Users == nil {
		return nil, errors.New("invalid Deplyt access state or PhotoLib origin has changed")
	}
	for id, user := range state.Users {
		if id <= 0 || id != user.ID || user.Username == "" {
			return nil, errors.New("invalid Deplyt access account")
		}
	}
	registry.users = state.Users
	return registry, nil
}

func (s *accessRegistry) allowed(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return id == s.admin || s.users[id].Granted
}

func (s *accessRegistry) register(user photoLibUser) error {
	if user.ID == s.admin {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.users[user.ID]
	account := accessAccount{user.ID, user.Username, user.DisplayName, previous.Granted}
	if previous == account {
		return nil
	}
	return s.save(account)
}

func (s *accessRegistry) list() []accessAccount {
	s.mu.Lock()
	defer s.mu.Unlock()
	accounts := make([]accessAccount, 0, len(s.users))
	for id, user := range s.users {
		if id != s.admin {
			accounts = append(accounts, user)
		}
	}
	slices.SortFunc(accounts, func(a, b accessAccount) int {
		if a.Granted != b.Granted {
			if a.Granted {
				return 1
			}
			return -1
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return accounts
}

func (s *accessRegistry) update(id int64, grant bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	account, exists := s.users[id]
	if !exists || id == s.admin {
		return os.ErrNotExist
	}
	if account.Granted == grant {
		return nil
	}
	account.Granted = grant
	return s.save(account)
}

// Keep the in-memory state aligned with the file if a write fails.
func (s *accessRegistry) save(account accessAccount) error {
	next := maps.Clone(s.users)
	next[account.ID] = account
	data, err := json.MarshalIndent(accessState{Issuer: s.issuer, Users: next}, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(s.path), ".access-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("write access state: %w", err)
	}
	if err := os.Rename(file.Name(), s.path); err != nil {
		return err
	}
	s.users = next
	return nil
}
