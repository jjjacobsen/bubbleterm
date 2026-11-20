package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

const (
	uiChats = iota
	uiHistory
	uiCompose
	uiReact
)

const historyPageSize = 50

var tapbacks = [...]string{"love", "like", "dislike", "laugh", "emphasis", "question"}

type chatsLoaded struct {
	chats []chat
	err   error
}

type historyLoaded struct {
	id, limit int
	messages  []message
	more      bool
	err       error
}

type actionFinished struct {
	id   int
	text string
	err  error
}

type watchChanged struct{}
type watchEnded struct{ err error }
type watchRetry struct{}
type catchUp struct{}

type historyRow struct {
	messageIndex int
	text         string
}

type ui struct {
	b      *backend
	events chan struct{}
	done   <-chan struct{}

	width, height                   int
	focus                           int
	chats                           []chat
	chatIndex, chatTop              int
	messages                        []message
	rows                            []historyRow
	rowIndex, rowTop                int
	drafts                          map[int][]rune
	draftCursor                     int
	target                          message
	chatsBusy, chatsAgain           bool
	historyBusy, historyAgain       bool
	historyLimit                    int
	moreHistory                     bool
	writing, reconnecting           bool
	chatsErr, historyErr, actionErr error
	watchErr                        error
}

func main() {
	host := flag.String("host", "mac", "SSH hostname for Messages")
	flag.Parse()
	b := &backend{host: *host}
	if err := errors.Join(runUI(b), b.close()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runUI(b *backend) error {
	done := make(chan struct{})
	defer close(done)
	m := &ui{b: b, events: make(chan struct{}, 1), done: done, drafts: make(map[int][]rune)}
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

func (m *ui) Init() tea.Cmd {
	return tea.Batch(m.loadChats(), m.watch(), m.waitForWatch(), catchUpLater())
}

func (m *ui) watch() tea.Cmd {
	return func() tea.Msg {
		return watchEnded{m.b.watch(func() {
			select {
			case m.events <- struct{}{}:
			default:
			}
		})}
	}
}

func (m *ui) waitForWatch() tea.Cmd {
	return func() tea.Msg {
		select {
		case <-m.events:
			return watchChanged{}
		case <-m.done:
			return nil
		}
	}
}

// The watch contract has no ready event. Periodic reads cover the gap before it connects.
func catchUpLater() tea.Cmd {
	return tea.Tick(30*time.Second, func(time.Time) tea.Msg { return catchUp{} })
}

func (m *ui) loadChats() tea.Cmd {
	if m.chatsBusy {
		m.chatsAgain = true
		return nil
	}
	m.chatsBusy, m.chatsAgain = true, false
	return func() tea.Msg {
		chats, err := m.b.chats()
		return chatsLoaded{chats, err}
	}
}

func (m *ui) loadHistory() tea.Cmd {
	if len(m.chats) == 0 {
		return nil
	}
	if m.historyBusy {
		m.historyAgain = true
		return nil
	}
	m.historyBusy, m.historyAgain = true, false
	id, limit := m.chats[m.chatIndex].ID, m.historyLimit
	return func() tea.Msg {
		messages, more, err := m.b.history(id, limit)
		return historyLoaded{id, limit, messages, more, err}
	}
}

func (m *ui) refresh() tea.Cmd {
	return tea.Batch(m.loadChats(), m.loadHistory())
}

func (m *ui) resetHistory() tea.Cmd {
	m.messages, m.rows = nil, nil
	m.historyLimit, m.moreHistory = historyPageSize, false
	m.rowIndex, m.rowTop, m.historyErr = 0, 0, nil
	if m.focus == uiCompose || m.focus == uiReact {
		m.focus = uiHistory
	}
	if len(m.chats) == 0 {
		m.focus = uiChats
	}
	return m.loadHistory()
}

func (m *ui) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	defer m.keepSelectionVisible()
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.setHistory(m.messages)
	case chatsLoaded:
		m.chatsBusy, m.chatsErr = false, msg.err
		var history, again tea.Cmd
		if msg.err == nil {
			hadChat := len(m.chats) > 0
			var id int
			if hadChat {
				id = m.chats[m.chatIndex].ID
			}
			m.chats = msg.chats
			index := slices.IndexFunc(m.chats, func(c chat) bool { return hadChat && c.ID == id })
			if index < 0 {
				m.chatIndex = min(m.chatIndex, max(0, len(m.chats)-1))
				history = m.resetHistory()
			} else {
				m.chatIndex = index
			}
		}
		if m.chatsAgain {
			again = m.loadChats()
		}
		return m, tea.Batch(history, again)
	case historyLoaded:
		m.historyBusy = false
		if len(m.chats) > 0 && m.chats[m.chatIndex].ID == msg.id && m.historyLimit == msg.limit {
			m.historyErr = msg.err
			if msg.err == nil {
				m.moreHistory = msg.more
				messages := msg.messages
				// Keep the loaded prefix when newer arrivals shift the window while browsing.
				if msg.more && m.rowIndex < len(m.rows)-1 {
					overlap := slices.IndexFunc(m.messages, func(item message) bool { return item.ID == messages[0].ID })
					if overlap > 0 {
						messages = slices.Concat(m.messages[:overlap], messages)
					}
				}
				m.setHistory(messages)
			}
		}
		if m.historyAgain {
			return m, m.loadHistory()
		}
	case watchChanged:
		m.reconnecting, m.watchErr = false, nil
		return m, tea.Batch(m.waitForWatch(), m.refresh())
	case watchEnded:
		m.reconnecting, m.watchErr = true, msg.err
		return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg { return watchRetry{} })
	case watchRetry:
		m.reconnecting, m.watchErr = false, nil
		return m, tea.Batch(m.watch(), m.refresh())
	case catchUp:
		return m, tea.Batch(catchUpLater(), m.refresh())
	case actionFinished:
		m.writing, m.actionErr = false, msg.err
		if msg.err != nil {
			return m, nil
		}
		if msg.text != "" && string(m.drafts[msg.id]) == msg.text {
			delete(m.drafts, msg.id)
			if len(m.chats) > 0 && m.chats[m.chatIndex].ID == msg.id {
				m.draftCursor = 0
			}
		}
		return m, m.refresh()
	case tea.KeyMsg:
		return m, m.key(msg)
	}
	return m, nil
}

func (m *ui) key(k tea.KeyMsg) tea.Cmd {
	key := k.String()
	switch key {
	case "ctrl+c":
		return tea.Quit
	case "ctrl+r":
		return m.refresh()
	}
	if len(m.chats) == 0 {
		return nil
	}
	id := m.chats[m.chatIndex].ID
	switch m.focus {
	case uiCompose:
		return m.compose(id, k)
	case uiReact:
		if key == "esc" {
			m.focus = uiHistory
		} else if !m.writing && len(key) == 1 && key[0] >= '1' && key[0] <= '6' {
			target, kind := m.target, tapbacks[key[0]-'1']
			m.focus, m.writing, m.actionErr = uiHistory, true, nil
			return func() tea.Msg { return actionFinished{id: id, err: m.b.react(id, target, kind)} }
		}
	case uiChats:
		switch key {
		case "tab", "enter", "right":
			m.focus = uiHistory
		case "i":
			m.focus, m.draftCursor = uiCompose, len(m.drafts[id])
		default:
			next := moveIndex(key, m.chatIndex, len(m.chats), m.bodyHeight())
			if next != m.chatIndex {
				m.chatIndex = next
				return m.resetHistory()
			}
		}
	case uiHistory:
		switch key {
		case "tab", "esc", "left":
			m.focus = uiChats
		case "i":
			m.focus, m.draftCursor = uiCompose, len(m.drafts[id])
		case "o":
			if m.moreHistory {
				limit := len(m.messages) + historyPageSize
				if limit > m.historyLimit || m.historyErr != nil {
					m.historyLimit = max(limit, m.historyLimit)
					return m.loadHistory()
				}
			}
		case "r":
			if !m.writing && len(m.rows) > 0 {
				m.focus, m.target = uiReact, m.messages[m.rows[m.rowIndex].messageIndex]
			}
		default:
			m.rowIndex = moveIndex(key, m.rowIndex, len(m.rows), m.bodyHeight())
		}
	}
	return nil
}

func (m *ui) compose(id int, k tea.KeyMsg) tea.Cmd {
	draft, cursor := m.drafts[id], m.draftCursor
	switch k.Type {
	case tea.KeyEsc, tea.KeyTab:
		m.focus = uiHistory
	case tea.KeyEnter:
		text := string(draft)
		if !m.writing && strings.TrimSpace(text) != "" {
			m.writing, m.actionErr = true, nil
			return func() tea.Msg { return actionFinished{id, text, m.b.send(id, text)} }
		}
	case tea.KeyLeft:
		m.draftCursor = max(0, cursor-1)
	case tea.KeyRight:
		m.draftCursor = min(len(draft), cursor+1)
	case tea.KeyHome:
		m.draftCursor = 0
	case tea.KeyEnd:
		m.draftCursor = len(draft)
	case tea.KeyBackspace:
		if cursor > 0 {
			m.drafts[id], m.draftCursor = slices.Delete(draft, cursor-1, cursor), cursor-1
		}
	case tea.KeyDelete:
		if cursor < len(draft) {
			m.drafts[id] = slices.Delete(draft, cursor, cursor+1)
		}
	case tea.KeyRunes, tea.KeySpace:
		if !k.Alt || k.Paste {
			text := []rune(cleanText(string(k.Runes)))
			m.drafts[id], m.draftCursor = slices.Insert(draft, cursor, text...), cursor+len(text)
		}
	}
	return nil
}

func moveIndex(key string, index, size, page int) int {
	switch key {
	case "up", "k":
		index--
	case "down", "j":
		index++
	case "pgup":
		index -= max(1, page-1)
	case "pgdown":
		index += max(1, page-1)
	case "home":
		index = 0
	case "end":
		index = size - 1
	}
	return min(max(0, index), max(0, size-1))
}

func (m *ui) setHistory(messages []message) {
	follow := m.rowIndex >= len(m.rows)-1
	var selectedID, offset int
	if !follow {
		index := m.rows[m.rowIndex].messageIndex
		selectedID = m.messages[index].ID
		for row := m.rowIndex - 1; row >= 0 && m.rows[row].messageIndex == index; row-- {
			offset++
		}
	}
	width := max(1, m.historyWidth()-2)
	m.messages, m.rows = messages, m.rows[:0]
	selected := -1
	for i, item := range messages {
		sender := oneLine(item.Sender)
		if item.FromMe {
			sender = "You"
		}
		text := cleanText(item.Text)
		if text == "" {
			text = "[No text]"
		}
		if sender != "" {
			text = sender + ": " + text
		}
		lines := strings.Split(ansi.Wrap(text, width, ""), "\n")
		if !follow && item.ID == selectedID {
			selected = len(m.rows) + min(offset, len(lines)-1)
		}
		for _, line := range lines {
			m.rows = append(m.rows, historyRow{i, line})
		}
	}
	if follow {
		m.rowIndex = max(0, len(m.rows)-1)
	} else if selected >= 0 {
		m.rowTop += selected - m.rowIndex
		m.rowIndex = selected
	} else {
		m.rowIndex = min(m.rowIndex, max(0, len(m.rows)-1))
	}
}

func (m *ui) sidebarWidth() int {
	if m.width < 72 {
		return 0
	}
	return min(30, m.width/3)
}

func (m *ui) historyWidth() int {
	if sidebar := m.sidebarWidth(); sidebar > 0 {
		return m.width - sidebar - 3
	}
	return m.width
}

func (m *ui) keepSelectionVisible() {
	height := max(1, m.bodyHeight())
	m.chatTop = scrollTop(m.chatTop, m.chatIndex, height, len(m.chats))
	m.rowTop = scrollTop(m.rowTop, m.rowIndex, height, len(m.rows))
}

func scrollTop(top, index, height, size int) int {
	return min(max(0, size-height), max(index-height+1, min(top, index)))
}

func (m *ui) View() string {
	if m.width < 1 || m.height < 1 {
		return ""
	}
	footer := m.footer()
	height := max(0, m.height-1-len(footer))
	title := "History"
	if len(m.chats) > 0 {
		title = chatName(m.chats[m.chatIndex])
	}
	var lines []string
	if sidebar := m.sidebarWidth(); sidebar > 0 {
		lines = append(lines, pad("Chats", sidebar)+" │ "+ansi.Truncate(title, m.historyWidth(), ""))
		for i := range height {
			lines = append(lines, pad(m.chatLine(i), sidebar)+" │ "+ansi.Truncate(m.historyLine(i), m.historyWidth(), ""))
		}
	} else if m.focus == uiChats {
		lines = append(lines, "Chats")
		for i := range height {
			lines = append(lines, m.chatLine(i))
		}
	} else {
		lines = append(lines, title)
		for i := range height {
			lines = append(lines, m.historyLine(i))
		}
	}
	lines = append(lines, footer...)
	for i, line := range lines {
		lines[i] = ansi.Truncate(line, m.width, "")
	}
	return strings.Join(lines, "\n")
}

func (m *ui) chatLine(row int) string {
	if len(m.chats) == 0 && row == 0 {
		if m.chatsBusy && m.chatsErr == nil {
			return "Loading chats…"
		}
		return "No chats"
	}
	index := m.chatTop + row
	if index >= len(m.chats) {
		return ""
	}
	return marker(index == m.chatIndex) + chatName(m.chats[index])
}

func (m *ui) historyLine(row int) string {
	if len(m.rows) == 0 && len(m.chats) > 0 && row == 0 {
		if m.historyBusy && m.historyErr == nil {
			return "Loading messages…"
		}
		return "No messages"
	}
	index := m.rowTop + row
	if index >= len(m.rows) {
		return ""
	}
	line := m.rows[index]
	selected := m.focus != uiChats && line.messageIndex == m.rows[m.rowIndex].messageIndex
	return marker(selected) + line.text
}

func (m *ui) bodyHeight() int {
	return max(0, m.height-1-len(m.footer()))
}

func (m *ui) footer() []string {
	if m.width < 1 || m.height < 2 {
		return nil
	}
	var lines []string
	switch m.focus {
	case uiChats:
		lines = append(lines, "Enter Open   ↑↓ Select   Ctrl+C Quit")
	case uiHistory:
		keys := "i Compose   r React   Esc Chats   ↑↓ Scroll"
		if m.moreHistory {
			keys = "o Older   " + keys
		}
		lines = append(lines, keys)
	case uiCompose:
		draft := m.drafts[m.chats[m.chatIndex].ID]
		before := strings.ReplaceAll(string(draft[:m.draftCursor]), "\n", "↵")
		after := strings.ReplaceAll(string(draft[m.draftCursor:]), "\n", "↵")
		before = ansi.TruncateLeft(before, max(0, ansi.StringWidth(before)-max(0, m.width-3)), "")
		lines = append(lines, "> "+before+"▏"+after, "Enter Send   Esc Back")
	case uiReact:
		lines = strings.Split(ansi.Wrap("1 Love  2 Like  3 Dislike  4 Laugh  5 Emphasize  6 Question  Esc Back", m.width, ""), "\n")
	}
	if status := m.status(); status != "" {
		lines = append(lines, status)
	}
	return lines[:min(len(lines), max(1, m.height-2))]
}

func (m *ui) status() string {
	switch {
	case m.actionErr != nil:
		return "Action: " + oneLine(m.actionErr.Error())
	case m.chatsErr != nil:
		return "Chats: " + oneLine(m.chatsErr.Error())
	case m.historyErr != nil:
		return "History: " + oneLine(m.historyErr.Error())
	case m.historyBusy && m.moreHistory && m.historyLimit > len(m.messages):
		return "Loading older messages…"
	case m.writing:
		return "Working…"
	case m.reconnecting && m.watchErr != nil:
		return "Reconnecting: " + oneLine(m.watchErr.Error())
	case m.reconnecting:
		return "Reconnecting…"
	}
	return ""
}

func marker(selected bool) string {
	if selected {
		return "> "
	}
	return "  "
}

func pad(s string, width int) string {
	s = ansi.Truncate(s, width, "")
	return s + strings.Repeat(" ", width-ansi.StringWidth(s))
}

func chatName(c chat) string {
	if name := oneLine(c.Name); name != "" {
		return name
	}
	return fmt.Sprintf("Chat %d", c.ID)
}

func oneLine(s string) string {
	return strings.ReplaceAll(cleanText(s), "\n", " ")
}

func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t':
			return ' '
		case r == '\n' || !unicode.IsControl(r):
			return r
		default:
			return -1
		}
	}, s)
}
