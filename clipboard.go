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

// Bubble Tea already distinguishes legacy CR (Enter) and LF (Ctrl+J).
// It exposes CSI-u keys, including Ghostty's Shift+Enter binding, as unknown CSI messages.
func terminalKey(msg tea.Msg) tea.Msg {
	if _, ok := msg.(tea.KeyMsg); ok {
		return msg
	}
	if seq, ok := msg.(fmt.Stringer); ok {
		switch seq.String() {
		case "?CSI[49 51 117]?", "?CSI[49 51 59 49 117]?": // 13u, 13;1u
			return tea.KeyMsg{Type: tea.KeyEnter}
		case "?CSI[49 51 59 50 117]?": // 13;2u
			return shiftEnterMsg{}
		case "?CSI[49 48 54 59 53 117]?": // 106;5u
			return tea.KeyMsg{Type: tea.KeyCtrlJ}
		case "?CSI[49 48 55 59 53 117]?": // 107;5u
			return tea.KeyMsg{Type: tea.KeyCtrlK}
		}
	}
	return msg
}
