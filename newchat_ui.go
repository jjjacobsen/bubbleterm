package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

var errHiddenChat = errors.New("chat is hidden. Unhide it with H then h first")

type newChatModal struct {
	directory  *directory
	query      messageDraft
	options    []recipientOption
	active     int
	recipients []recipientOption
	draft      messageDraft
	composing  bool
	loading    bool
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
	s.loading, s.err = true, nil
	return func() tea.Msg {
		dir, err := m.b.directory()
		return directoryLoaded{s, dir, err}
	}
}

func (m *ui) finishDirectory(msg directoryLoaded) {
	if m.newChat != msg.modal {
		return
	}
	m.newChat.loading = false
	if msg.err != nil {
		m.newChat.err = fmt.Errorf("load recipients: %w", msg.err)
		return
	}
	var older []chat
	for _, c := range msg.dir.chats {
		if m.hidden.contains(c.chat) && chatIndex(m.latestChats, c.ID) < 0 {
			older = append(older, c.chat)
		}
	}
	if err := m.hidden.refresh(older); err != nil {
		m.actionErr = fmt.Errorf("refresh hidden chats: %w", err)
	}
	m.newChat.directory = &msg.dir
	m.newChat.updateOptions(m.hidden)
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
	if s.directory == nil && !s.loading && k.Type == tea.KeyCtrlR {
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
			s.updateOptions(m.hidden)
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
			s.updateOptions(m.hidden)
		}
	}
	return nil
}

func (s *newChatModal) updateOptions(hidden *chatStore) {
	s.active, s.options, s.err = -1, nil, nil
	query := strings.TrimSpace(string(s.query.text))
	if query == "" {
		return
	}
	for _, option := range s.directory.suggestions(query) {
		if option.Chat == nil || len(s.recipients) == 0 && !hidden.contains(*option.Chat) {
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
	s.updateOptions(m.hidden)
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
	addresses, hidden := s.addresses(), m.hidden.chats
	s.sending, s.err, m.writing = true, nil, true
	return func() tea.Msg {
		c, err := m.b.startChat(addresses, text, hidden)
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
	m.sentReads = append(m.sentReads, msg.chat.ID)
	return tea.Batch(m.openDirectoryChat(msg.chat), m.loadChats())
}

func (m *ui) openDirectoryChat(c chat) tea.Cmd {
	if m.hidden.contains(c) {
		m.newChat.err = errHiddenChat
		return nil
	}
	m.newChat, m.directoryChat, m.actionErr = nil, &c, nil
	m.rebuildChats()
	m.chatIndex = chatIndex(m.chats, c.ID)
	return m.openSelectedChat(true)
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

func (s *newChatModal) keys() string {
	switch {
	case s.composing:
		return "Enter Send  Esc Cancel  Shift+Enter New Line  Tab Recipients"
	case s.directory == nil:
		return "Ctrl+R Retry  Esc Cancel"
	case strings.TrimSpace(string(s.query.text)) != "":
		return "Enter Select  Tab/↑↓ Choose  Esc Cancel"
	case len(s.recipients) > 0:
		return "Enter Continue  Backspace Remove  Esc Cancel"
	default:
		return "Type a name, phone, or email  Esc Cancel"
	}
}

func (m *ui) newChatView() string {
	s := m.newChat
	title := "New chat"
	input, prefix := &s.query, "To> "
	if s.composing {
		title, input, prefix = "New chat · First message", &s.draft, "> "
	}
	bodyHeight := max(1, m.height-1)
	room := bodyHeight
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
		chipLines := strings.Split(ansi.Wrap(fmt.Sprintf("To (%d): %s", len(chips), strings.Join(chips, " ")), m.width, ""), "\n")
		chipLines = chipLines[:min(len(chipLines), room/2)]
		lines = append(lines, chipLines...)
		room -= len(chipLines)
	}
	if m.width <= len(prefix) {
		prefix = ""
	}
	if s.composing {
		lines = append(lines, input.visibleLines(prefix, m.width, room, true)...)
	} else {
		query := input.visibleLines(prefix, m.width, min(2, max(1, room-1)), true)
		lines = append(lines, query...)
		count := room - len(query)
		top := scrollTop(0, max(0, s.active), count, len(s.options))
		for i := top; i < min(top+count, len(s.options)); i++ {
			lines = append(lines, marker(i == s.active)+recipientLabel(s.options[i], m.width-2))
		}
	}
	for i, line := range lines {
		lines[i] = ansi.Truncate(line, m.width, "")
	}
	lines = append(lines, make([]string, bodyHeight-len(lines))...)
	if m.height >= 2 {
		lines = append(lines, m.footer())
	}
	return strings.Join(lines, "\n")
}
