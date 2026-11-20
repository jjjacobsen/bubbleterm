package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type themeAccent string

func watchAccent(done <-chan struct{}, changes chan<- themeAccent) {
	resolver, err := exec.LookPath("omarchy-theme-color")
	if err != nil {
		return
	}
	path := filepath.Join(os.Getenv("HOME"), ".local/state/omarchy/current/theme/colors.toml")
	var last os.FileInfo
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		info, err := os.Stat(path)
		if err == nil && (last == nil || !os.SameFile(last, info) || last.ModTime() != info.ModTime()) {
			out, err := exec.Command(resolver, "accent").Output()
			color, parseErr := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(string(out)), "#"), 16, 24)
			if err == nil && parseErr == nil {
				select {
				case changes <- themeAccent(fmt.Sprintf("\x1b[38;2;%d;%d;%dm", color>>16, color>>8&255, color&255)):
				case <-done:
					return
				}
				last = info
			}
		}
		select {
		case <-tick.C:
		case <-done:
			return
		}
	}
}

func (m *ui) waitForAccent() tea.Msg {
	select {
	case accent := <-m.accents:
		return accent
	case <-m.done:
		return nil
	}
}
