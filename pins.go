package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

type pins struct {
	file  string
	chats []chat
}

func newPins(host string) (*pins, error) {
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
		return nil, fmt.Errorf("pin directory is not private: %s", dir)
	}

	p := &pins{file: filepath.Join(dir, fmt.Sprintf("pins-%x.json", sha256.Sum256([]byte(host))))}
	info, err = os.Lstat(p.file)
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("pin file is not a private regular file: %s", p.file)
	}
	data, err := os.ReadFile(p.file)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &p.chats); err != nil {
		return nil, fmt.Errorf("decode pins from %s: %w", p.file, err)
	}
	return p, nil
}

func (p *pins) index(c chat) int {
	return slices.IndexFunc(p.chats, func(pin chat) bool { return pin.ID == c.ID })
}

func (p *pins) contains(c chat) bool {
	return p.index(c) >= 0
}

func (p *pins) toggle(c chat) error {
	chats := slices.Clone(p.chats)
	if i := p.index(c); i >= 0 {
		chats = slices.Delete(chats, i, i+1)
	} else {
		chats = append(chats, c)
	}

	out, err := os.CreateTemp(filepath.Dir(p.file), ".pins-*")
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
	p.chats = chats
	return nil
}

func (p *pins) order(latest []chat) []chat {
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
		}
		ordered = append(ordered, c)
	}
	for _, c := range latest {
		if _, ok := byID[c.ID]; ok {
			ordered = append(ordered, c)
		}
	}
	return ordered
}
