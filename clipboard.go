package main

import (
	"fmt"
	"os/exec"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

func copyText(text string) error {
	cmd := exec.Command("wl-copy", "--type", "text/plain;charset=utf-8")
	cmd.Stdin = strings.NewReader(text)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("copy to clipboard: %w", err)
	}
	return nil
}

func clipboardMessage(m message) string {
	if m.UnsentAt != nil && m.Text == "[Unsent message]" {
		return ""
	}
	text := m.Text
	if strings.ContainsRune(text, '\ufffc') {
		text = strings.TrimSpace(strings.ReplaceAll(text, "\ufffc", ""))
	}
	if len(m.Links) == 0 {
		return text
	}
	inline := textURL.FindAllString(m.Text, -1)
	seen := make(map[string]bool)
	for _, link := range m.Links {
		if seen[link] || slices.ContainsFunc(inline, func(raw string) bool {
			trimmed := trimLink(raw)
			return trimmed == link || strings.HasPrefix(raw, link) && trimmed == trimLink(link)
		}) {
			continue
		}
		seen[link] = true
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += link
	}
	return text
}

type shiftEnterMsg struct{}

// Call from tea.WithFilter. Omarchy Ghostty binds Shift+Enter to CSI 13;2u.
// Bubble Tea v1 exposes this as an unexported CSI message, not a KeyMsg.
func clipboardKey(msg tea.Msg) tea.Msg {
	if _, ok := msg.(tea.KeyMsg); ok {
		return msg
	}
	if seq, ok := msg.(fmt.Stringer); ok && seq.String() == "?CSI[49 51 59 50 117]?" {
		return shiftEnterMsg{}
	}
	return msg
}
