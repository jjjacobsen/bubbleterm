package main

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func (m *ui) hiddenKey(k tea.KeyMsg) tea.Cmd {
	key := k.String()
	switch key {
	case "esc", "H":
		if !k.Paste {
			m.focus, m.actionErr = uiChats, nil
		}
	case "h":
		if len(m.hidden.chats) > 0 && !k.Paste {
			m.toggleHidden(m.hidden.chats[m.hiddenIndex])
		}
	default:
		m.hiddenIndex = moveIndex(strings.ToLower(key), m.hiddenIndex, len(m.hidden.chats), m.layout().bodyHeight)
	}
	return nil
}

func (m *ui) toggleHidden(c chat) {
	restore := m.focus == uiHidden
	if err := m.hidden.toggle(c); err != nil {
		m.actionErr = fmt.Errorf("toggle hidden chat: %w", err)
		return
	}
	m.actionErr = nil
	if restore && chatIndex(m.latestChats, c.ID) < 0 {
		if m.openedChat != nil && m.openedChat.ID == c.ID {
			c.Unread = m.openedChat.Unread
			*m.openedChat = c
			c.Unread = false
		}
		if i := chatIndex(m.restoredChats, c.ID); i >= 0 {
			m.restoredChats[i] = c
		} else {
			m.restoredChats = append(m.restoredChats, c)
		}
	}
	m.hiddenIndex = min(m.hiddenIndex, max(0, len(m.hidden.chats)-1))
	if m.reselectChats() {
		m.clearHistory()
	}
}

func (m *ui) hiddenView() string {
	lines := []string{"Hidden chats"}
	index, top := m.searchSelection(m.hiddenIndex, m.hiddenTop)
	footer := m.footer()
	for row := range m.height - 1 - len(footer) {
		i := top + row
		switch {
		case len(m.hidden.chats) == 0 && row == 0:
			lines = append(lines, "No hidden chats")
		case i < len(m.hidden.chats):
			c := m.hidden.chats[i]
			mark := chatMarker(i == index, m.unreadChats[c.ID])
			name := chatName(c)
			if m.search != nil {
				if label, ok := m.search.chatLabels[i]; ok {
					name = m.search.highlight(i, label, m.width-ansi.StringWidth(mark))
				}
			}
			lines = append(lines, mark+name)
		default:
			lines = append(lines, "")
		}
	}
	lines = append(lines, footer...)
	for i, line := range lines {
		lines[i] = ansi.Truncate(line, m.width, "")
	}
	return strings.Join(lines, "\n")
}
