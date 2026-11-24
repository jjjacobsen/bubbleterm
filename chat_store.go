package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

type chatStore struct {
	file  string
	mu    sync.RWMutex // protects publication to the watch goroutine
	chats []chat
}

func newChatStore(host, kind string) (*chatStore, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(base, "bubbleterm")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("chat store directory is not private: %s", dir)
	}

	p := &chatStore{file: filepath.Join(dir, fmt.Sprintf("%s-%x.json", kind, sha256.Sum256([]byte(host))))}
	info, err = os.Lstat(p.file)
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("chat store is not a private regular file: %s", p.file)
	}
	data, err := os.ReadFile(p.file)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &p.chats); err != nil {
		return nil, fmt.Errorf("decode chat store %s: %w", p.file, err)
	}
	return p, nil
}

func chatIndex(chats []chat, id int) int {
	return slices.IndexFunc(chats, func(c chat) bool { return c.ID == id })
}

func (p *chatStore) contains(c chat) bool {
	return p.containsID(c.ID)
}

func (p *chatStore) containsID(id int) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return chatIndex(p.chats, id) >= 0
}

func (p *chatStore) toggle(c chat) error {
	chats := slices.Clone(p.chats)
	if i := chatIndex(chats, c.ID); i >= 0 {
		chats = slices.Delete(chats, i, i+1)
	} else {
		c.Unread = false
		chats = append(chats, c)
	}

	return p.save(chats)
}

func (p *chatStore) refresh(current []chat) error {
	var chats []chat
	for _, c := range current {
		i := chatIndex(p.chats, c.ID)
		c.Unread = false
		if i >= 0 && c != p.chats[i] {
			if chats == nil {
				chats = slices.Clone(p.chats)
			}
			chats[i] = c
		}
	}
	if chats != nil {
		return p.save(chats)
	}
	return nil
}

func (p *chatStore) save(chats []chat) error {
	out, err := os.CreateTemp(filepath.Dir(p.file), ".chat-store-*")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	writeErr := json.NewEncoder(out).Encode(chats)
	if err := errors.Join(writeErr, out.Close()); err != nil {
		return err
	}
	if err := os.Rename(out.Name(), p.file); err != nil {
		return err
	}
	p.mu.Lock()
	p.chats = chats
	p.mu.Unlock()
	return nil
}

func (p *chatStore) order(latest []chat) []chat {
	if len(p.chats) == 0 {
		return latest
	}
	byID := make(map[int]chat, len(latest))
	for _, c := range latest {
		byID[c.ID] = c
	}
	ordered := make([]chat, 0, len(latest)+len(p.chats))
	for _, c := range p.chats {
		if current, ok := byID[c.ID]; ok {
			c = current
			delete(byID, c.ID)
		} else {
			c.Unread = false
		}
		ordered = append(ordered, c)
	}
	slices.SortStableFunc(ordered, func(a, b chat) int {
		return b.LastMessageAt.Compare(a.LastMessageAt)
	})
	for _, c := range latest {
		if _, ok := byID[c.ID]; ok {
			ordered = append(ordered, c)
		}
	}
	return ordered
}
