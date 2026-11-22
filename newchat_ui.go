package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type newChatModal struct {
	directory  *directory
	query      messageDraft
	options    []recipientOption
	active     int
	recipients []recipientOption
	draft      messageDraft
	composing  bool
	sending    bool
	uncertain  bool
	err        error
}

type directoryLoaded struct {
	modal *newChatModal
	dir   directory
	err   error
}

type newChatStarted struct {
	chat chat
	err  error
}

func (m *ui) openNewChat() tea.Cmd {
	m.newChat = &newChatModal{active: -1}
	return m.loadDirectory()
}

func (m *ui) loadDirectory() tea.Cmd {
	s := m.newChat
	s.err = nil
	return func() tea.Msg {
		dir, err := m.b.directory()
		return directoryLoaded{s, dir, err}
	}
}

func (m *ui) finishDirectory(msg directoryLoaded) {
	if m.newChat != msg.modal {
		return
	}
	if msg.err != nil {
		m.newChat.err = fmt.Errorf("load recipients: %w", msg.err)
		return
	}
	m.newChat.directory = &msg.dir
	m.newChat.updateOptions()
}

func (m *ui) newChatKey(k tea.KeyMsg) tea.Cmd {
	s := m.newChat
	if s.sending {
		return nil
	}
	if k.Type == tea.KeyEsc {
		m.newChat = nil
		return nil
	}
	if s.directory == nil && s.err != nil && k.Type == tea.KeyCtrlR {
		return m.loadDirectory()
	}
	if s.composing {
		switch k.Type {
		case tea.KeyEnter:
			return m.sendFirstMessage()
		case tea.KeyTab:
			s.composing, s.err = false, nil
		case tea.KeyCtrlJ:
			s.draft.insert("\n")
			s.err = nil
		default:
			before := len(s.draft.text)
			s.draft.editText(k)
			if len(s.draft.text) != before {
				s.err = nil
			}
		}
		return nil
	}
	switch k.Type {
	case tea.KeyTab, tea.KeyDown:
		s.moveOption(1)
	case tea.KeyShiftTab, tea.KeyUp:
		s.moveOption(-1)
	case tea.KeyEnter:
		if s.directory != nil {
			return m.chooseRecipient()
		}
	case tea.KeyBackspace:
		if len(s.query.text) == 0 && len(s.recipients) > 0 {
			s.recipients = s.recipients[:len(s.recipients)-1]
			s.updateOptions()
			return nil
		}
		fallthrough
	default:
		before := len(s.query.text)
		if k.Paste {
			k.Runes = []rune(oneLine(string(k.Runes)))
		}
		s.query.editText(k)
		if len(s.query.text) != before && s.directory != nil {
			s.updateOptions()
		}
	}
	return nil
}

func (s *newChatModal) updateOptions() {
	s.active, s.options, s.err = -1, nil, nil
	query := strings.TrimSpace(string(s.query.text))
	if query == "" {
		return
	}
	options := s.directory.suggestions(query)
	if len(s.recipients) == 0 {
		s.options = options
		return
	}
	for _, option := range options {
		if option.Chat == nil {
			s.options = append(s.options, option)
		}
	}
}

func (s *newChatModal) moveOption(delta int) {
	if n := len(s.options); n > 0 {
		if s.active < 0 && delta < 0 {
			s.active = n - 1
		} else {
			s.active = (s.active + delta + n) % n
		}
		s.err = nil
	}
}

func (m *ui) chooseRecipient() tea.Cmd {
	s := m.newChat
	query := strings.TrimSpace(string(s.query.text))
	if query == "" {
		if len(s.recipients) == 0 {
			s.err = errors.New("add at least one recipient")
			return nil
		}
		c, found, err := s.directory.match(s.addresses())
		if err != nil {
			s.err = err
			return nil
		}
		if found {
			return m.openDirectoryChat(c)
		}
		s.composing, s.err, s.query = true, nil, messageDraft{}
		return nil
	}
	if s.active >= 0 {
		return m.useRecipient(s.options[s.active])
	}
	address, addressErr := normalizeAddress(query)
	if addressErr == nil {
		for _, option := range s.options {
			if option.Chat == nil && option.Address == address {
				return m.useRecipient(option)
			}
		}
		return m.useRecipient(recipientOption{Address: address})
	}
	exact := -1
	for i, option := range s.options {
		if strings.EqualFold(query, strings.TrimSpace(option.Name)) {
			if exact >= 0 {
				s.err = errors.New("multiple exact matches. Choose one with Tab or ↑/↓")
				return nil
			}
			exact = i
		}
	}
	switch {
	case exact >= 0:
		return m.useRecipient(s.options[exact])
	case len(s.options) == 1 && s.options[0].Chat == nil:
		return m.useRecipient(s.options[0])
	case len(s.options) > 0:
		s.err = errors.New("choose a match with Tab or ↑/↓")
	default:
		s.err = fmt.Errorf("no matches: %w", addressErr)
	}
	return nil
}

func (m *ui) useRecipient(option recipientOption) tea.Cmd {
	if option.Chat != nil {
		return m.openDirectoryChat(*option.Chat)
	}
	s := m.newChat
	if slices.ContainsFunc(s.recipients, func(existing recipientOption) bool { return existing.Address == option.Address }) {
		s.err = errors.New("recipient already added")
		return nil
	}
	s.recipients = append(s.recipients, option)
	s.query = messageDraft{}
	s.updateOptions()
	return nil
}

func (s *newChatModal) addresses() []string {
	addresses := make([]string, len(s.recipients))
	for i, option := range s.recipients {
		addresses[i] = option.Address
	}
	return addresses
}

func (m *ui) sendFirstMessage() tea.Cmd {
	s := m.newChat
	if s.uncertain {
		s.err = errors.New("send outcome unknown. Close this draft and check Messages before retrying")
		return nil
	}
	text := string(s.draft.text)
	if strings.TrimSpace(text) == "" {
		s.err = errors.New("write a message before sending")
		return nil
	}
	if m.writing {
		s.err = errors.New("another action is still in progress")
		return nil
	}
	addresses := s.addresses()
	s.sending, s.err, m.writing = true, nil, true
	return func() tea.Msg {
		c, err := m.b.startChat(addresses, text)
		return newChatStarted{c, err}
	}
}

func (m *ui) finishNewChat(msg newChatStarted) tea.Cmd {
	s := m.newChat
	s.sending, m.writing = false, false
	if msg.err != nil {
		s.uncertain = errors.Is(msg.err, errNewChatUncertain)
		s.err = msg.err
		return nil
	}
	return tea.Batch(m.openDirectoryChat(msg.chat), m.loadChats())
}

func (m *ui) openDirectoryChat(c chat) tea.Cmd {
	m.newChat, m.openedChat, m.focus, m.actionErr = nil, &c, uiHistory, nil
	m.rebuildChats()
	m.chatIndex = slices.IndexFunc(m.chats, func(item chat) bool { return item.ID == c.ID })
	history := m.resetHistory()
	m.requestViewChat()
	return history
}

func recipientLabel(option recipientOption, width int) string {
	name, address := oneLine(option.Name), oneLine(option.Address)
	if option.Chat != nil {
		kind := "Chat"
		if option.Chat.IsGroup {
			kind = "Group"
		}
		address = fmt.Sprintf("%s %d", kind, option.Chat.ID)
	}
	if name == "" || name == address || width <= ansi.StringWidth(address)+3 {
		return address
	}
	return ansi.Truncate(name, width-ansi.StringWidth(address)-3, "…") + " · " + address
}

func (m *ui) newChatView() string {
	s := m.newChat
	title := "New chat"
	var keys, status string
	input, prefix := &s.query, "To> "
	switch {
	case s.sending:
		title, keys, status = "New chat · First message", "", "Sending first message…"
		input, prefix = &s.draft, "> "
	case s.composing:
		title, keys = "New chat · First message", "Enter Send  Shift+Enter New Line  Tab Recipients  Esc Cancel"
		input, prefix = &s.draft, "> "
	case s.directory == nil:
		if s.err == nil {
			keys, status = "Esc Cancel", "Loading recipients…"
		} else {
			keys = "Ctrl+R Retry  Esc Cancel"
		}
	case strings.TrimSpace(string(s.query.text)) != "":
		keys = "Enter Select  Tab/↑↓ Choose  Esc Cancel"
	case len(s.recipients) > 0:
		keys = "Enter Continue  Backspace Remove  Esc Cancel"
	default:
		keys = "Type a name, phone, or email  Esc Cancel"
	}
	if s.err != nil {
		status = oneLine(s.err.Error())
	}
	wrap := func(text string, limit int) []string {
		if text == "" || limit == 0 {
			return nil
		}
		lines := strings.Split(ansi.Wrap(text, m.width, ""), "\n")
		return lines[:min(len(lines), limit)]
	}
	hints := wrap(keys, min(3, (m.height+1)/3))
	room := m.height - len(hints)
	notice := wrap(status, min(3, room-1))
	room -= len(notice)
	var lines []string
	if room >= 3 {
		lines = append(lines, title)
		room--
	}
	if len(s.recipients) > 0 && room > 1 {
		chips := make([]string, len(s.recipients))
		for i, option := range s.recipients {
			chips[i] = "[" + recipientLabel(option, m.width-2) + "]"
		}
		chipLines := wrap(fmt.Sprintf("To (%d): %s", len(chips), strings.Join(chips, " ")), room/2)
		lines = append(lines, chipLines...)
		room -= len(chipLines)
	}
	if m.width <= len(prefix) {
		prefix = ""
	}
	if s.composing {
		lines = append(lines, input.visibleLines(prefix, m.width, room)...)
	} else {
		query := input.visibleLines(prefix, m.width, min(2, max(1, room-1)))
		lines = append(lines, query...)
		count := room - len(query)
		top := scrollTop(0, max(0, s.active), count, len(s.options))
		for i := top; i < min(top+count, len(s.options)); i++ {
			lines = append(lines, marker(i == s.active)+recipientLabel(s.options[i], m.width-2))
		}
	}
	lines = append(lines, notice...)
	lines = append(lines, hints...)
	for i, line := range lines {
		lines[i] = ansi.Truncate(line, m.width, "")
	}
	return strings.Join(lines, "\n")
}
