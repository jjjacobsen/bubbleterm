package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

const (
	uiChats = iota
	uiHidden
	uiHistory
	uiCompose
	uiReact
	uiEdit
)

const (
	historyPageSize = 50
	previewWidth    = 24
	previewHeight   = 6
	previewWorkers  = 2
)

var tapbacks = [...]string{"love", "like", "dislike", "laugh", "emphasis", "question"}

type chatsLoaded struct {
	chats  []chat
	unread map[int]bool
	err    error
}

type historyLoaded struct {
	id, limit, version int
	messages           []message
	more               bool
	err                error
}

type chatViewed struct {
	afterSend bool
	err       error
}

type draftKey struct {
	chatID int
	guid   string
}

type messageDraft struct {
	text     []rune
	cursor   int
	file     *draftFile
	input    string
	inputErr error
}

func (d *messageDraft) insert(text string) {
	runes := []rune(cleanText(text))
	d.text = slices.Insert(d.text, d.cursor, runes...)
	d.cursor += len(runes)
}

type draftInput struct {
	key   draftKey
	text  string
	file  *draftFile
	err   error
	ready chan struct{} // non-nil for a clipboard read
}

type actionFinished struct {
	draft  *messageDraft
	chatID int // only set for sends
	err    error
}

type messageEdit struct {
	target   draftKey
	original string
	created  time.Time
	draft    messageDraft
}

type editFinished struct{ err error }

type fileUpload struct {
	size, sent int64
	updates    chan int64
	lastReport time.Time
}

type uploadProgress struct {
	upload *fileUpload
	sent   int64
}

func (u *fileUpload) wait(done <-chan struct{}) tea.Msg {
	select {
	case sent, ok := <-u.updates:
		if ok {
			return uploadProgress{u, sent}
		}
	case <-done:
	}
	return nil
}

func (u *fileUpload) report(sent int64) {
	now := time.Now()
	if sent < u.size && now.Sub(u.lastReport) < 100*time.Millisecond {
		return
	}
	u.lastReport = now
	// Keep the newest update without making the transfer wait for the UI.
	select {
	case u.updates <- sent:
	default:
		select {
		case <-u.updates:
		default:
		}
		u.updates <- sent
	}
}

type photoOpened struct{ err error }
type videoFinished struct{ err error }
type linkOpened struct{ err error }
type clipboardCopied struct{ err error }

type photoPreview struct{ rows []string }

type previewLoaded struct {
	key     string
	preview *photoPreview
	png     []byte
	err     error
}

type watchChanged struct{}
type watchEnded struct{ err error }
type watchRetry struct{}
type catchUp struct{}

type historyRow struct {
	messageIndex    int // -1 for a date label
	attachmentIndex int // -1 for message text or links
	linkNumber      int // 1-based, zero for non-link rows
	photoRow        int // 1 through previewHeight for thumbnail rows
	searchStart     int // byte offset in the unwrapped history search block
	text            string
}

type ui struct {
	b       *backend
	g       *graphics
	events  chan watchChanged
	done    <-chan struct{}
	accent  themeAccent
	accents chan themeAccent
	started time.Time

	previews     map[string]*photoPreview
	previewsBusy int

	width, height                   int
	focus                           int
	pins, hidden                    *chatStore
	latestChats, chats              []chat
	unreadChats                     map[int]bool
	openedChat                      *chat  // keep a chat opened outside the recent list
	restoredChats                   []chat // keep older unhidden chats available for this session
	pinnedCount, chatIndex, chatTop int
	hiddenIndex, hiddenTop          int
	viewPending, viewing            int // chat IDs of a pending open and a running mark-read
	sentReads                       []int
	viewErr                         error
	terminalBlurred                 bool
	messages                        []message
	rows                            []historyRow
	historySearch                   []historySearchBlock
	rowIndex, rowTop                int
	search                          *paneSearch
	newChat                         *newChatModal
	helpOpen                        bool
	helpTop                         int
	drafts                          map[draftKey]*messageDraft
	composeDraft                    draftKey
	replyContext                    string
	editor                          *messageEdit
	clipboardRead                   *draftInput
	sending                         *messageDraft
	upload                          *fileUpload
	target                          message
	chatsBusy, chatsAgain           bool
	historyBusy, historyAgain       bool
	historyLimit, historyVersion    int
	historyLatestID                 int
	historyLatestAt                 time.Time
	moreHistory                     bool
	writing, reconnecting           bool
	videoActive                     bool
	chatsErr, historyErr, actionErr error
	watchErr                        error
	lastWatchID                     int // cursor used only by the watch command
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
	pins, err := newChatStore(b.host, "pins")
	if err != nil {
		return fmt.Errorf("load pinned chats: %w", err)
	}
	hidden, err := newChatStore(b.host, "hidden")
	if err != nil {
		return fmt.Errorf("load hidden chats: %w", err)
	}
	done := make(chan struct{})
	g := newGraphics()
	defer g.close()
	m := &ui{b: b, g: g, events: make(chan watchChanged, 1), done: done, started: time.Now().Truncate(time.Millisecond), pins: pins, hidden: hidden, drafts: make(map[draftKey]*messageDraft)}
	defer func() {
		close(done)
		m.cleanupDrafts()
	}()
	m.accents = make(chan themeAccent)
	go watchAccent(done, m.accents)
	_, err = tea.NewProgram(m, tea.WithAltScreen(), tea.WithReportFocus(), tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
		switch msg.(type) {
		case tea.QuitMsg, tea.InterruptMsg:
			g.close()
		}
		return terminalKey(msg)
	})).Run()
	return err
}

func (m *ui) cleanupDrafts() {
	// A clipboard read can create a temporary file after the event loop stops.
	if read := m.clipboardRead; read != nil {
		<-read.ready
		if read.file != nil {
			read.file.cleanup()
		}
	}
	for _, draft := range m.drafts {
		if draft.file != nil {
			draft.file.cleanup()
		}
	}
}

func (m *ui) Init() tea.Cmd {
	return tea.Batch(tea.SetWindowTitle("BubbleTerm"), m.loadChats(), m.watch(), m.waitForWatch(), m.waitForAccent, catchUpLater())
}

func (m *ui) watch() tea.Cmd {
	return func() tea.Msg {
		command, _ := exec.LookPath("notify-send")
		notify := m.b.notificationHandler(command, m.hidden)
		startedAt := time.Now().Truncate(time.Millisecond)
		return watchEnded{m.b.watch(func(ctx context.Context, row json.RawMessage) {
			// A queued refresh already covers changes that arrive before it runs.
			select {
			case m.events <- watchChanged{}:
			default:
			}
			var event watchEvent
			if json.Unmarshal(row, &event) == nil && event.newMessage(startedAt, m.lastWatchID) {
				m.lastWatchID = event.ID
				if !*event.FromMe && notify != nil {
					notify(ctx, event)
				}
			}
		})}
	}
}

func (m *ui) waitForWatch() tea.Cmd {
	return func() tea.Msg {
		select {
		case change := <-m.events:
			return change
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
		chats, unread, err := m.b.chats()
		return chatsLoaded{chats, unread, err}
	}
}

func (m *ui) loadHistory() tea.Cmd {
	if m.focus == uiHidden || len(m.chats) == 0 {
		return nil
	}
	if m.historyBusy {
		m.historyAgain = true
		return nil
	}
	m.historyBusy, m.historyAgain = true, false
	if m.historyLimit == 0 {
		m.historyLimit = historyPageSize
	}
	id, limit, version := m.chats[m.chatIndex].ID, m.historyLimit, m.historyVersion
	return func() tea.Msg {
		messages, more, err := m.b.history(id, limit)
		return historyLoaded{id, limit, version, messages, more, err}
	}
}

func (m *ui) refresh() tea.Cmd {
	return tea.Batch(m.loadChats(), m.loadHistory())
}

func (m *ui) resetHistory() tea.Cmd {
	m.clearHistory()
	return m.loadHistory()
}

func (m *ui) clearHistory() {
	m.viewPending, m.viewErr, m.historyAgain = 0, nil, false
	m.messages, m.rows, m.historySearch = nil, nil, nil
	if m.focus == uiHistory {
		m.search = nil
	}
	clear(m.previews)
	m.historyVersion++
	m.historyLimit, m.moreHistory = 0, false
	m.historyLatestID, m.historyLatestAt = 0, time.Time{}
	m.rowIndex, m.rowTop, m.historyErr = 0, 0, nil
	if m.focus == uiCompose || m.focus == uiReact || m.focus == uiEdit {
		m.focus = uiHistory
	}
	m.editor = nil
	if len(m.chats) == 0 && m.focus != uiHidden {
		m.focus = uiChats
	}
}

func (m *ui) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch k := msg.(type) {
	case tea.KeyMsg:
		key := k.String()
		if key == "ctrl+c" {
			return m, tea.Quit
		}
		if m.helpOpen {
			if !k.Paste {
				m.helpKey(key)
			}
			return m, nil
		}
		if m.focus != uiCompose && m.focus != uiEdit && m.search == nil && m.newChat == nil {
			switch key {
			case "q":
				return m, tea.Quit
			case "?":
				if !k.Paste {
					m.helpOpen, m.helpTop = true, 0
					return m, nil
				}
			}
		}
	case shiftEnterMsg:
		if m.helpOpen {
			return m, nil
		}
	}
	wasHidden := m.focus == uiHidden
	cmd := m.update(msg)
	view := m.maybeViewChat()
	m.keepSelectionVisible()
	if wasHidden || m.focus == uiHidden {
		return m, tea.Batch(cmd, view)
	}
	return m, tea.Batch(cmd, view, m.loadPreviews())
}

func (m *ui) followHistory(messages []message) bool {
	// History is ordered by date. Older pages and reaction updates do not
	// advance the newest message. Consume it even when follow is paused.
	for _, item := range slices.Backward(messages) {
		if item.UnsentAt != nil {
			continue
		}
		if item.ID <= m.historyLatestID || item.CreatedAt.Before(m.historyLatestAt) {
			return false
		}
		m.historyLatestID, m.historyLatestAt = item.ID, item.CreatedAt
		// Use the session start, not the last poll: a message can be
		// created before a refresh but synced to this Mac after it.
		return !item.CreatedAt.Before(m.started) && m.search == nil && m.newChat == nil &&
			(m.focus == uiHistory || (m.focus == uiChats && m.sidebarWidth() > 0))
	}
	return false
}

func (m *ui) atLatestMessage() bool {
	return len(m.rows) > 0 && m.rows[m.rowIndex].messageIndex == len(m.messages)-1
}

func (m *ui) requestViewChat() {
	c := m.chats[m.chatIndex]
	if c.Unread && c.ID != m.viewing && (len(m.rows) == 0 || m.atLatestMessage()) {
		m.viewPending, m.viewErr, m.actionErr = c.ID, nil, nil
	}
}

func (m *ui) maybeViewChat() tea.Cmd {
	if m.viewing == 0 && len(m.sentReads) > 0 {
		id := m.sentReads[0]
		m.sentReads = m.sentReads[1:]
		if m.viewPending == id {
			m.viewPending = 0
		}
		// Only the Mac can tell whether the destination is still unread.
		return m.markRead(id, true)
	}
	id := m.viewPending
	if id == 0 {
		return nil
	}
	if m.focus != uiHistory || m.search != nil || m.terminalBlurred || len(m.chats) == 0 ||
		m.chats[m.chatIndex].ID != id || !m.chats[m.chatIndex].Unread ||
		(len(m.rows) > 0 && !m.atLatestMessage()) {
		m.viewPending = 0
		return nil
	}
	// Refreshes may resolve a chat as already read or move the latest message.
	if m.viewing != 0 || m.chatsBusy || m.historyBusy {
		return nil
	}
	m.viewPending = 0
	if m.chatsErr != nil || m.historyErr != nil {
		return nil
	}
	if !m.atLatestMessage() {
		m.viewErr = errors.New("latest message is not available")
		return nil
	}
	return m.markRead(id, false)
}

func (m *ui) markRead(id int, afterSend bool) tea.Cmd {
	m.viewing = id
	return func() tea.Msg { return chatViewed{afterSend, m.b.viewChat(id)} }
}

func (m *ui) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.FocusMsg:
		m.terminalBlurred = false
		return m.refresh()
	case tea.BlurMsg:
		m.terminalBlurred = true
	case themeAccent:
		m.accent = msg
		return m.waitForAccent
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.setHistory(m.messages, m.rowIndex == len(m.rows)-1)
		if m.search != nil && m.focus == uiHistory {
			m.updateSearch()
		}
	case chatsLoaded:
		m.chatsBusy, m.chatsErr = false, msg.err
		var history, again tea.Cmd
		if msg.err == nil {
			m.latestChats, m.unreadChats = msg.chats, msg.unread
			if err := m.pins.refresh(m.latestChats); err != nil {
				m.actionErr = fmt.Errorf("refresh pinned chats: %w", err)
			}
			if err := m.hidden.refresh(m.latestChats); err != nil {
				m.actionErr = fmt.Errorf("refresh hidden chats: %w", err)
			}
			if m.reselectChats() {
				history = m.resetHistory()
			}
			if len(m.chats) > 0 && !m.chats[m.chatIndex].Unread {
				m.viewErr = nil
			}
			if m.search != nil && m.searchingChats() {
				m.updateSearch()
			}
		}
		if m.chatsAgain {
			again = m.loadChats()
		}
		return tea.Batch(history, again)
	case historyLoaded:
		m.historyBusy = false
		if msg.version == m.historyVersion && msg.limit == m.historyLimit && len(m.chats) > 0 && m.chats[m.chatIndex].ID == msg.id {
			m.historyErr = msg.err
			if msg.err == nil {
				m.moreHistory = msg.more
				messages := msg.messages
				follow := m.followHistory(messages)
				// Keep the loaded prefix when newer arrivals shift the window while browsing.
				if msg.more && m.rowIndex < len(m.rows)-1 {
					overlap := slices.IndexFunc(m.messages, func(item message) bool { return item.ID == messages[0].ID })
					if overlap > 0 {
						messages = slices.Concat(m.messages[:overlap], messages)
					}
				}
				m.setHistory(messages, follow)
				if m.search != nil && m.focus == uiHistory {
					m.updateSearch()
				}
			}
		}
		if m.historyAgain {
			return m.loadHistory()
		}
	case chatViewed:
		id := m.viewing
		m.viewing = 0
		if msg.err == nil {
			if m.openedChat != nil && m.openedChat.ID == id {
				m.openedChat.Unread = false
			}
			return m.loadChats()
		}
		if msg.afterSend {
			m.actionErr = fmt.Errorf("message sent to chat %d, but could not mark it read: %w", id, msg.err)
		} else if len(m.chats) > 0 && m.chats[m.chatIndex].ID == id && m.chats[m.chatIndex].Unread {
			m.viewErr = msg.err
		}
	case previewLoaded:
		m.previewsBusy--
		if msg.err == nil && m.previews[msg.key] == msg.preview {
			rows, err := m.g.place(msg.key, msg.png, previewWidth, previewHeight)
			if err == nil {
				msg.preview.rows = rows
			}
		}
	case photoOpened:
		m.writing, m.actionErr = false, msg.err
	case videoFinished:
		m.videoActive = false
		if msg.err != nil {
			m.actionErr = msg.err
		}
	case linkOpened:
		if msg.err != nil {
			m.actionErr = msg.err
		}
	case clipboardCopied:
		if msg.err != nil {
			m.actionErr = msg.err
		}
	case directoryLoaded:
		m.finishDirectory(msg)
	case newChatStarted:
		return m.finishNewChat(msg)
	case shiftEnterMsg:
		if m.newChat != nil {
			return m.newChatKey(tea.KeyMsg{Type: tea.KeyCtrlJ})
		}
		if m.focus == uiCompose || m.focus == uiEdit {
			return m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'\n'}})
		}
		if m.focus == uiHistory && m.search == nil {
			return m.copy(m.selectedLink())
		}
	case watchChanged:
		m.reconnecting, m.watchErr = false, nil
		return tea.Batch(m.waitForWatch(), m.refresh())
	case watchEnded:
		m.reconnecting, m.watchErr = true, msg.err
		return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return watchRetry{} })
	case watchRetry:
		m.reconnecting, m.watchErr = false, nil
		return tea.Batch(m.watch(), m.refresh())
	case catchUp:
		return tea.Batch(catchUpLater(), m.refresh())
	case *draftInput:
		m.finishDraftInput(msg)
	case uploadProgress:
		if m.upload == msg.upload {
			m.upload.sent = msg.sent
			return func() tea.Msg { return msg.upload.wait(m.done) }
		}
	case editFinished:
		m.writing, m.actionErr = false, msg.err
		if msg.err != nil {
			return nil
		}
		if m.focus == uiEdit {
			m.focus, m.editor = uiHistory, nil
		}
		return m.refresh()
	case actionFinished:
		m.writing, m.actionErr = false, msg.err
		m.sending, m.upload = nil, nil
		if msg.err != nil {
			return nil
		}
		if draft := msg.draft; draft != nil {
			if draft.file != nil {
				draft.file.cleanup()
			}
			*draft = messageDraft{}
			m.sentReads = append(m.sentReads, msg.chatID)
		}
		return m.refresh()
	case tea.KeyMsg:
		return m.key(msg)
	}
	return nil
}

func (m *ui) key(k tea.KeyMsg) tea.Cmd {
	if m.newChat != nil {
		return m.newChatKey(k)
	}
	if m.search != nil {
		return m.searchKey(k)
	}
	key := k.String()
	if key == "/" && (m.searchingChats() || m.focus == uiHistory) {
		m.startSearch()
		return nil
	}
	if m.focus == uiHidden {
		return m.hiddenKey(k)
	}
	if m.focus == uiChats && key == "H" && !k.Paste {
		m.focus, m.actionErr, m.historyAgain = uiHidden, nil, false
		return nil
	}
	if key == "ctrl+r" {
		return m.refresh()
	}
	if m.focus == uiChats && (key == "n" || key == "N") {
		return m.openNewChat()
	}
	if len(m.chats) == 0 {
		return nil
	}
	id := m.chats[m.chatIndex].ID
	if (m.focus == uiChats || m.focus == uiHistory) && (key == "m" || key == "M") && !k.Paste {
		if m.chats[m.chatIndex].Unread && m.viewing == 0 {
			m.viewPending, m.viewErr, m.actionErr = 0, nil, nil
			return m.markRead(id, false)
		}
		return nil
	}
	if (m.focus == uiChats || m.focus == uiHistory) && (key == "J" || key == "K") {
		return m.moveChat(strings.ToLower(key))
	}
	switch m.focus {
	case uiCompose:
		return m.compose(k)
	case uiEdit:
		return m.edit(k)
	case uiReact:
		switch {
		case key == "esc":
			m.focus = uiHistory
		case !m.writing && key == "7" && m.target.MyReaction != "":
			return m.react(id, "remove")
		case !m.writing && len(key) == 1 && key[0] >= '1' && key[0] <= '6':
			return m.react(id, tapbacks[key[0]-'1'])
		}
	case uiChats:
		switch key {
		case "tab", "enter", "right":
			m.focus = uiHistory
			m.requestViewChat()
			return m.loadHistory()
		case "i":
			m.startCompose(draftKey{chatID: id}, "")
		case "p", "P":
			if err := m.pins.toggle(m.chats[m.chatIndex]); err != nil {
				m.actionErr = fmt.Errorf("toggle pin: %w", err)
				return nil
			}
			m.actionErr = nil
			if m.reselectChats() {
				return m.resetHistory()
			}
		case "h":
			if !k.Paste {
				m.toggleHidden(m.chats[m.chatIndex])
			}
		default:
			return m.moveChat(key)
		}
	case uiHistory:
		switch key {
		case "tab", "esc", "left":
			m.focus = uiChats
		case "i":
			m.startCompose(draftKey{chatID: id}, "")
		case "R":
			if len(m.rows) > 0 {
				m.startReply(id, m.messages[m.rows[m.rowIndex].messageIndex])
			}
		case "e":
			if !m.writing && len(m.rows) > 0 {
				m.startEdit(id, m.messages[m.rows[m.rowIndex].messageIndex])
			}
		case "c", "C":
			if len(m.rows) > 0 {
				return m.copy(clipboardMessage(m.messages[m.rows[m.rowIndex].messageIndex]))
			}
		case "shift+enter":
			return m.copy(m.selectedLink())
		case "enter":
			a := m.selectedAttachment()
			if a.isPhoto() {
				if !m.writing {
					m.writing, m.actionErr = true, nil
					return func() tea.Msg { return photoOpened{m.b.openPhoto(a)} }
				}
			} else if a.isVideo() {
				if !m.videoActive {
					m.videoActive, m.actionErr = true, nil
					return func() tea.Msg { return videoFinished{m.b.playVideo(a)} }
				}
			} else if link := m.selectedLink(); link != "" {
				m.actionErr = nil
				return func() tea.Msg {
					if err := exec.Command("xdg-open", link).Run(); err != nil {
						return linkOpened{fmt.Errorf("open link: %w", err)}
					}
					return linkOpened{}
				}
			} else if len(m.rows) == 0 || m.rows[m.rowIndex].attachmentIndex < 0 {
				m.requestViewChat()
			}
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
				row := m.rows[m.rowIndex]
				m.focus, m.target, m.actionErr = uiReact, m.messages[row.messageIndex], nil
				if row.attachmentIndex >= 0 {
					m.target.SelectedAttachment = &m.target.Attachments[row.attachmentIndex]
				}
			}
		default:
			m.moveHistory(key)
		}
	}
	return nil
}

func (m *ui) react(id int, kind string) tea.Cmd {
	target := m.target
	m.focus, m.writing, m.actionErr = uiHistory, true, nil
	return func() tea.Msg { return actionFinished{err: m.b.react(id, target, kind)} }
}

func (m *ui) copy(text string) tea.Cmd {
	if text == "" {
		return nil
	}
	m.actionErr = nil
	return func() tea.Msg { return clipboardCopied{copyText(text)} }
}

func (m *ui) selectedAttachment() attachment {
	if len(m.rows) == 0 {
		return attachment{}
	}
	row := m.rows[m.rowIndex]
	if row.linkNumber > 0 {
		return attachment{}
	}
	attachments := m.messages[row.messageIndex].Attachments
	if row.attachmentIndex >= 0 {
		return attachments[row.attachmentIndex]
	}
	// Keep the photo shortcut on message text. Videos require selection.
	for _, a := range attachments {
		if a.isPhoto() {
			return a
		}
	}
	return attachment{}
}

func (m *ui) selectedLink() string {
	if len(m.rows) == 0 {
		return ""
	}
	row := m.rows[m.rowIndex]
	links := m.messages[row.messageIndex].Links
	if row.linkNumber > 0 {
		return links[row.linkNumber-1]
	}
	if row.attachmentIndex < 0 && len(links) == 1 {
		return links[0]
	}
	return ""
}

// Keep the same selection when possible. The caller decides whether to load history.
func (m *ui) reselectChats() bool {
	selected := 0
	if len(m.chats) > 0 {
		selected = m.chats[m.chatIndex].ID
	}
	m.rebuildChats()
	if i := chatIndex(m.chats, selected); i >= 0 {
		m.chatIndex = i
		return false
	}
	m.chatIndex = min(m.chatIndex, max(0, len(m.chats)-1))
	return selected != 0 || len(m.chats) > 0
}

func (m *ui) rebuildChats() {
	latest := m.latestChats
	if opened := m.openedChat; opened != nil {
		if i := chatIndex(latest, opened.ID); i >= 0 {
			*opened = latest[i]
		} else {
			opened.Unread = m.unreadChats[opened.ID]
			latest = append(slices.Clip(latest), *opened)
		}
	}
	if len(m.restoredChats) > 0 {
		latest = slices.Clone(latest)
		for i, c := range m.restoredChats {
			j := chatIndex(latest, c.ID)
			if j >= 0 {
				m.restoredChats[i] = latest[j]
			} else {
				j = len(latest)
				latest = append(latest, c)
			}
			latest[j].Unread = m.unreadChats[c.ID]
		}
	}
	m.chats = m.chats[:0]
	for _, c := range m.pins.order(latest) {
		if !m.hidden.contains(c) {
			m.chats = append(m.chats, c)
		}
	}
	m.pinnedCount = 0
	for _, c := range m.chats {
		if !m.pins.contains(c) {
			break
		}
		m.pinnedCount++
	}
}

func (m *ui) moveChat(key string) tea.Cmd {
	next := moveIndex(key, m.chatIndex, len(m.chats), m.bodyHeight())
	if next == m.chatIndex {
		return nil
	}
	m.chatIndex = next
	return m.resetHistory()
}

func (m *ui) moveHistory(key string) {
	if len(m.rows) == 0 {
		return
	}
	index := m.rowIndex
	switch key {
	case "up", "k", "down", "j":
		step := 1
		if key == "up" || key == "k" {
			step = -1
		}
		current := m.rows[index]
		for i := index + step; i >= 0 && i < len(m.rows); i += step {
			next := m.rows[i]
			if next.messageIndex < 0 || next.sameItem(current) {
				continue
			}
			for i > 0 && next.sameItem(m.rows[i-1]) {
				i--
			}
			m.rowIndex = i
			break
		}
		return
	}
	index = moveIndex(key, index, len(m.rows), m.bodyHeight())
	if m.rows[index].messageIndex < 0 {
		if index > 0 && index < m.rowIndex {
			index--
		} else {
			index++
		}
	}
	m.rowIndex = index
}

func (r historyRow) sameItem(other historyRow) bool {
	return r.messageIndex == other.messageIndex && r.attachmentIndex == other.attachmentIndex && r.linkNumber == other.linkNumber
}

func (m *ui) startCompose(key draftKey, replyContext string) {
	m.focus, m.composeDraft, m.replyContext = uiCompose, key, replyContext
	if m.drafts[key] == nil {
		m.drafts[key] = &messageDraft{}
	}
}

func (m *ui) startReply(id int, item message) {
	guid := item.GUID
	if item.Thread.GUID != "" {
		if !item.Thread.canContinue() {
			m.actionErr = errors.New("reply helper cannot continue this message part")
			return
		}
		// Messages keeps replies under the original message, not the selected reply.
		guid = item.Thread.GUID
		var root *message
		if i := slices.IndexFunc(m.messages, func(item message) bool { return item.GUID == guid }); i >= 0 {
			root = &m.messages[i]
		}
		m.startCompose(draftKey{chatID: id, guid: guid}, threadLabel(item.Thread, root))
	} else if guid != "" {
		m.startCompose(draftKey{chatID: id, guid: guid}, replyMessageLabel(item))
	}
}

func (m *ui) startEdit(id int, item message) {
	if !item.Editable || !item.withinEditWindow() {
		m.actionErr = errors.New("select an editable iMessage you sent in the last 15 minutes")
		return
	}
	text := []rune(item.Text)
	m.editor = &messageEdit{
		target: draftKey{chatID: id, guid: item.GUID}, original: item.Text, created: item.CreatedAt,
		draft: messageDraft{text: text, cursor: len(text)},
	}
	m.focus, m.actionErr = uiEdit, nil
}

func (m *ui) edit(k tea.KeyMsg) tea.Cmd {
	if m.writing {
		return nil
	}
	editor := m.editor
	switch k.Type {
	case tea.KeyEsc, tea.KeyTab:
		m.focus, m.editor, m.actionErr = uiHistory, nil, nil
	case tea.KeyEnter:
		text := string(editor.draft.text)
		if text == editor.original {
			m.focus, m.editor, m.actionErr = uiHistory, nil, nil
			return nil
		}
		if strings.TrimSpace(text) == "" {
			m.actionErr = errors.New("edited message cannot be empty")
			return nil
		}
		if time.Since(editor.created) >= editWindow {
			m.actionErr = errors.New("the 15-minute edit window has expired")
			return nil
		}
		target := editor.target
		m.writing, m.actionErr = true, nil
		return func() tea.Msg { return editFinished{m.b.edit(target.chatID, target.guid, text)} }
	default:
		editor.draft.editText(k)
	}
	return nil
}

var errReplyAttachment = errors.New("thread replies do not support attachments")
var errReplaceAttachment = errors.New("remove the attachment with Ctrl+X first")

func (m *ui) compose(k tea.KeyMsg) tea.Cmd {
	key := m.composeDraft
	draft := m.drafts[key]
	if k.Type == tea.KeyEsc || k.Type == tea.KeyTab {
		m.focus = uiHistory
		return nil
	}
	if draft == m.sending || draft.input != "" {
		return nil
	}
	switch k.Type {
	case tea.KeyEnter:
		return m.sendDraft(key, draft)
	case tea.KeyCtrlV:
		m.actionErr = nil
		if m.clipboardRead != nil {
			draft.inputErr = errors.New("clipboard read in progress")
			return nil
		}
		draft.input, draft.inputErr = "Reading clipboard…", nil
		read := &draftInput{key: key, ready: make(chan struct{})}
		m.clipboardRead = read
		go func() {
			defer close(read.ready)
			read.text, read.file, read.err = pasteClipboard()
		}()
		return func() tea.Msg {
			<-read.ready
			return read
		}
	case tea.KeyCtrlO:
		m.actionErr = nil
		if key.guid != "" {
			draft.inputErr = errReplyAttachment
			return nil
		}
		if draft.file != nil {
			draft.inputErr = errReplaceAttachment
			return nil
		}
		draft.input, draft.inputErr = "Choosing file…", nil
		return func() tea.Msg {
			file, err := pickFile()
			return &draftInput{key: key, file: file, err: err}
		}
	case tea.KeyCtrlX:
		if draft.file != nil {
			draft.file.cleanup()
			draft.file, draft.inputErr, m.actionErr = nil, nil, nil
		}
	default:
		draft.editText(k)
	}
	return nil
}

func (draft *messageDraft) editText(k tea.KeyMsg) {
	cursor := draft.cursor
	switch k.Type {
	case tea.KeyLeft:
		draft.cursor = max(0, cursor-1)
	case tea.KeyRight:
		draft.cursor = min(len(draft.text), cursor+1)
	case tea.KeyHome:
		draft.cursor = 0
	case tea.KeyEnd:
		draft.cursor = len(draft.text)
	case tea.KeyBackspace:
		if cursor > 0 {
			draft.text, draft.cursor = slices.Delete(draft.text, cursor-1, cursor), cursor-1
		}
	case tea.KeyDelete:
		if cursor < len(draft.text) {
			draft.text = slices.Delete(draft.text, cursor, cursor+1)
		}
	case tea.KeyRunes, tea.KeySpace:
		if !k.Alt || k.Paste {
			draft.insert(string(k.Runes))
		}
	}
}

func (m *ui) finishDraftInput(msg *draftInput) {
	if msg.ready != nil {
		m.clipboardRead = nil
	}
	draft := m.drafts[msg.key]
	draft.input = ""
	err := msg.err
	if err == nil && msg.file != nil {
		switch {
		case msg.key.guid != "":
			err = errReplyAttachment
		case draft.file != nil:
			err = errReplaceAttachment
		default:
			draft.file = msg.file
		}
	}
	if err != nil {
		if msg.file != nil {
			msg.file.cleanup()
		}
		draft.inputErr = err
		return
	}
	draft.insert(msg.text)
}

func (m *ui) sendDraft(key draftKey, draft *messageDraft) tea.Cmd {
	if draft.file != nil && key.guid != "" {
		draft.inputErr = errReplyAttachment
		return nil
	}
	text := string(draft.text)
	if m.writing || (draft.file == nil && strings.TrimSpace(text) == "") {
		return nil
	}
	m.focus = uiHistory
	m.writing, m.sending, m.actionErr = true, draft, nil
	draft.inputErr = nil
	if draft.file != nil {
		file := *draft.file
		upload := &fileUpload{size: file.Size, updates: make(chan int64, 1)}
		m.upload = upload
		return tea.Batch(func() tea.Msg {
			defer close(upload.updates)
			return actionFinished{draft, key.chatID, m.b.sendFile(key.chatID, text, file, upload.report)}
		}, func() tea.Msg { return upload.wait(m.done) })
	}
	return func() tea.Msg {
		if key.guid != "" {
			return actionFinished{draft, key.chatID, m.b.reply(key.chatID, key.guid, text)}
		}
		return actionFinished{draft, key.chatID, m.b.send(key.chatID, text)}
	}
}

func moveIndex(key string, index, size, page int) int {
	switch key {
	case "up", "k":
		index--
	case "down", "j":
		index++
	case "ctrl+u":
		index -= max(1, page/2)
	case "ctrl+d":
		index += max(1, page/2)
	case "pgup":
		index -= max(1, page-1)
	case "pgdown":
		index += max(1, page-1)
	case "home", "<":
		index = 0
	case "end", ">":
		index = size - 1
	}
	return min(max(0, index), max(0, size-1))
}

func sameDay(a, b time.Time) bool {
	return a.Year() == b.Year() && a.YearDay() == b.YearDay()
}

func historyDateLabel(at, now time.Time) string {
	var date string
	switch {
	case sameDay(at, now):
		date = "Today"
	case sameDay(at, now.AddDate(0, 0, -1)):
		date = "Yesterday"
	case at.Year() == now.Year():
		date = at.Format("Jan 2")
	default:
		date = at.Format("Jan 2, 2006")
	}
	return date + " · " + at.Format("3:04 PM")
}

func historyLinkLabel(i, count int) string {
	if count == 1 {
		return "[Link]"
	}
	return fmt.Sprintf("[Link %d]", i+1)
}

func historyText(text string, links []string) string {
	text = cleanText(text)
	if strings.ContainsRune(text, '\ufffc') {
		text = strings.TrimSpace(strings.ReplaceAll(text, "\ufffc", ""))
	}
	if len(links) == 0 {
		return text
	}
	lines := slices.DeleteFunc(strings.Split(text, "\n"), func(line string) bool {
		return slices.Contains(links, strings.TrimSpace(line))
	})
	// Replace shared URL prefixes in longest-first order.
	order := make([]int, len(links))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(i, j int) int { return len(links[j]) - len(links[i]) })
	var pairs []string
	for _, i := range order {
		pairs = append(pairs, links[i], historyLinkLabel(i, len(links)))
	}
	return strings.NewReplacer(pairs...).Replace(strings.TrimSpace(strings.Join(lines, "\n")))
}

func replyLabel(author, text string) string {
	if author != "" {
		text = author + ": " + text
	}
	return ansi.Truncate(strings.Join(strings.Fields(oneLine(text)), " "), 72, "…")
}

func replyMessageLabel(item message) string {
	author := item.SenderName
	if item.FromMe {
		author = "You"
	}
	text := historyText(item.Text, item.Links)
	switch {
	case item.UnsentAt != nil:
		text = "[Unsent message]"
	case strings.TrimSpace(text) != "":
	case len(item.Links) > 0:
		text = "[Link]"
	case len(item.Attachments) > 0:
		text = "[Attachment]"
	default:
		text = "[No text]"
	}
	return replyLabel(author, text)
}

func threadLabel(thread threadOrigin, root *message) string {
	if root != nil {
		return replyMessageLabel(*root)
	}
	text := thread.Text
	if text == "" && thread.UnixDate > 0 {
		text = time.Unix(thread.UnixDate, 0).Local().Format("Jan 2, 2006 3:04 PM")
	}
	if text == "" {
		text = "message " + thread.GUID[:min(8, len(thread.GUID))]
	}
	return replyLabel(thread.Author, text)
}

func (m *ui) setHistory(messages []message, follow bool) {
	follow = follow || len(m.rows) == 0
	var old historyRow
	var selectedID, offset int
	if !follow {
		old = m.rows[m.rowIndex]
		selectedID = m.messages[old.messageIndex].ID
		if old.attachmentIndex < 0 && old.linkNumber == 0 {
			for row := m.rowIndex - 1; row >= 0 && m.rows[row].messageIndex == old.messageIndex; row-- {
				offset++
			}
		}
	}
	width := max(1, m.historyWidth()-2)
	thumbnails := m.thumbnailSpace()
	m.messages, m.rows = messages, m.rows[:0]
	m.historySearch = m.historySearch[:0]
	selected := -1
	now := time.Now()
	isGroup := len(m.chats) > 0 && m.chats[m.chatIndex].IsGroup
	var prev time.Time
	var prevSender string
	byGUID := make(map[string]*message, len(messages))
	for i := range messages {
		byGUID[messages[i].GUID] = &messages[i]
	}
	for i, item := range messages {
		at := item.CreatedAt.Local()
		if i == 0 || !sameDay(at, prev) || at.Sub(prev) >= time.Hour {
			m.rows = append(m.rows, historyRow{messageIndex: -1, text: historyDateLabel(at, now)})
			prevSender = ""
		}
		prev = at
		start := len(m.rows)
		if item.Thread.GUID != "" {
			label := threadLabel(item.Thread, byGUID[item.Thread.GUID])
			m.rows = append(m.rows, historyRow{messageIndex: i, attachmentIndex: -1, text: "↳ " + label})
		}
		var sender, senderID string
		if isGroup && !item.FromMe {
			senderID = item.Sender
			if senderID == "" {
				senderID = item.SenderName
			}
			if senderID != prevSender {
				sender = oneLine(item.SenderName)
				if sender == "" {
					sender = oneLine(item.Sender)
				}
			}
		}
		prevSender = senderID
		reaction := oneLine(item.MyReaction)
		if reaction != "" {
			reaction = "[Your " + reaction + "] "
		}
		if others := oneLine(item.OtherReactions); others != "" {
			reaction += "[" + others + "] "
		}
		text := historyText(item.Text, item.Links)
		searchable := text != "" && !(item.UnsentAt != nil && text == "[Unsent message]")
		if text == "" && len(item.Attachments) == 0 && len(item.Links) == 0 {
			text = "[No text]"
		}
		if text != "" {
			prefix := reaction
			if sender != "" {
				prefix += sender + ": "
			}
			m.appendHistoryText(i, prefix, text, width, searchable)
			sender, reaction = "", ""
		}
		restore := !follow && item.ID == selectedID
		if restore {
			selected = start + min(offset, max(0, len(m.rows)-start-1))
		}
		for j, link := range item.Links {
			if restore && j+1 == old.linkNumber {
				selected = len(m.rows)
			}
			prefix := reaction
			if sender != "" {
				prefix += sender + ": "
			}
			prefix += historyLinkLabel(j, len(item.Links)) + " "
			text := prefix + link
			row := len(m.rows)
			m.rows = append(m.rows, historyRow{messageIndex: i, attachmentIndex: -1, linkNumber: j + 1, text: text})
			m.historySearch = append(m.historySearch, historySearchBlock{text, len(prefix), row, row + 1})
			sender, reaction = "", ""
		}
		photos, videos := 0, 0
		for _, a := range item.Attachments {
			if a.isPhoto() {
				photos++
			} else if a.isVideo() {
				videos++
			}
		}
		photo, video := 0, 0
		for j, a := range item.Attachments {
			caption, height := "[Attachment]", 0
			if a.isPhoto() {
				photo++
				caption = "[Photo]"
				if photos > 1 {
					caption = fmt.Sprintf("[Photo %d/%d]", photo, photos)
				}
				if thumbnails {
					height = previewHeight
				}
			} else if a.isVideo() {
				video++
				caption = "[Video]"
				if videos > 1 {
					caption = fmt.Sprintf("[Video %d/%d]", video, videos)
				}
			}
			if restore && j == old.attachmentIndex {
				selected = len(m.rows) + min(old.photoRow, height)
			}
			if nameWidth := width - ansi.StringWidth(reaction) - len(caption) - 2; sender != "" && nameWidth > 0 {
				caption = ansi.Truncate(sender, nameWidth, "") + ": " + caption
			}
			m.rows = append(m.rows, historyRow{messageIndex: i, attachmentIndex: j, text: reaction + caption})
			sender, reaction = "", ""
			for k := 1; k <= height; k++ {
				m.rows = append(m.rows, historyRow{messageIndex: i, attachmentIndex: j, photoRow: k})
			}
		}
	}
	if follow {
		m.rowIndex = max(0, len(m.rows)-1)
	} else if selected >= 0 {
		m.rowTop += selected - m.rowIndex
		m.rowIndex = selected
	} else {
		m.rowIndex = min(m.rowIndex, max(0, len(m.rows)-1))
		if len(m.rows) > 0 && m.rows[m.rowIndex].messageIndex < 0 {
			m.rowIndex++
		}
	}
}

func (m *ui) appendHistoryText(index int, prefix, text string, width int, searchable bool) {
	source := prefix + text
	first, offset := len(m.rows), 0
	for _, line := range strings.Split(ansi.Wrap(source, width, ""), "\n") {
		// Wrapping can discard space at line boundaries. Locate each displayed fragment in order.
		start := strings.Index(source[offset:], line)
		if start >= 0 {
			start += offset
			offset = start + len(line)
		}
		m.rows = append(m.rows, historyRow{messageIndex: index, attachmentIndex: -1, text: line, searchStart: start})
	}
	if searchable {
		m.historySearch = append(m.historySearch, historySearchBlock{source, len(prefix), first, len(m.rows)})
	}
}

func (m *ui) thumbnailSpace() bool {
	return m.g.supported && m.historyWidth() >= previewWidth+2 && m.height >= previewHeight+4
}

func (m *ui) loadPreviews() tea.Cmd {
	// Keep the cache while a modal or search changes what is visible.
	if m.newChat != nil || m.search != nil {
		return nil
	}
	if !m.thumbnailSpace() || (m.sidebarWidth() == 0 && m.focus == uiChats) {
		clear(m.previews)
		return nil
	}

	// Keep rendered rows only while their thumbnail is visible. A new entry
	// has a new identity, so results from an earlier view are ignored.
	visible := make(map[string]*photoPreview)
	var cmds []tea.Cmd
	end := min(len(m.rows), m.rowTop+m.bodyHeight())
	for _, row := range m.rows[m.rowTop:end] {
		if row.photoRow == 0 {
			continue
		}
		a := m.messages[row.messageIndex].Attachments[row.attachmentIndex]
		key := a.key()
		if visible[key] != nil {
			continue
		}
		if p := m.previews[key]; p != nil {
			visible[key] = p
		} else if m.previewsBusy < previewWorkers {
			p := &photoPreview{}
			visible[key] = p
			m.previewsBusy++
			cmds = append(cmds, func() tea.Msg {
				png, err := m.b.preview(a)
				return previewLoaded{key, p, png, err}
			})
		}
	}
	m.previews = visible
	return tea.Batch(cmds...)
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

func (m *ui) chatRow(index int) int {
	if m.pinnedCount == 0 {
		return index
	}
	if index < m.pinnedCount {
		return index + 1
	}
	return index + 2
}

func (m *ui) scrollChats(top, index, height int) int {
	row := m.chatRow(index)
	top = scrollTop(top, row, height, m.chatRow(len(m.chats)-1)+1)
	if height > 1 && m.pinnedCount > 0 && top == row && (index == 0 || index == m.pinnedCount) {
		top--
	}
	return top
}

func (m *ui) keepSelectionVisible() {
	height := max(1, m.bodyHeight())
	m.chatTop = m.scrollChats(m.chatTop, m.chatIndex, height)
	if m.focus == uiHidden {
		m.hiddenTop = scrollTop(m.hiddenTop, m.hiddenIndex, height, len(m.hidden.chats))
	}
	m.rowTop = scrollTop(m.rowTop, m.rowIndex, height, len(m.rows))
	if len(m.rows) > 0 {
		start := m.rowIndex - m.rows[m.rowIndex].photoRow
		end := m.rowIndex
		if height > previewHeight && start+1 < len(m.rows) && m.rows[start+1].photoRow == 1 {
			end = start + previewHeight
			m.rowTop = min(start, max(m.rowTop, end-height+1))
		}
		if start > 0 && m.rowTop == start && m.rows[start-1].messageIndex < 0 && end-start+2 <= height {
			m.rowTop--
		}
	}
	if m.search != nil {
		m.keepSearchVisible(height)
	}
}

func scrollTop(top, index, height, size int) int {
	return min(max(0, size-height), max(index-height+1, min(top, index)))
}

func (m *ui) View() string {
	if m.width < 1 || m.height < 1 {
		return ""
	}
	if m.helpOpen {
		return m.helpView()
	}
	return m.mainView()
}

func (m *ui) mainView() string {
	if m.newChat != nil {
		return m.newChatView()
	}
	if m.focus == uiHidden {
		return m.hiddenView()
	}
	footer := m.footer()
	height := max(0, m.height-1-len(footer))
	title := "History"
	if len(m.chats) > 0 {
		c := m.chats[m.chatIndex]
		title = chatName(c)
		if c.Unread && m.sidebarWidth() == 0 {
			title = "• " + title
		}
	}
	var lines []string
	if sidebar := m.sidebarWidth(); sidebar > 0 {
		lines = append(lines, pad("Chats", sidebar)+" │ "+ansi.Truncate(title, m.historyWidth(), ""))
		for i := range height {
			lines = append(lines, pad(m.chatLine(i), sidebar)+" │ "+m.historyLine(i))
		}
	} else if m.focus == uiChats {
		lines = append(lines, ansi.Truncate("Chats", m.width, ""))
		for i := range height {
			lines = append(lines, ansi.Truncate(m.chatLine(i), m.width, ""))
		}
	} else {
		lines = append(lines, ansi.Truncate(title, m.width, ""))
		for i := range height {
			lines = append(lines, m.historyLine(i))
		}
	}
	for _, line := range footer {
		lines = append(lines, ansi.Truncate(line, m.width, ""))
	}
	return strings.Join(lines, "\n")
}

func (m *ui) chatLine(row int) string {
	if len(m.chats) == 0 && row == 0 {
		if len(m.hidden.chats) > 0 {
			return "No visible chats"
		}
		if m.chatsBusy && m.chatsErr == nil {
			return "Loading chats…"
		}
		return "No chats"
	}
	selected, top := m.chatIndex, m.chatTop
	if m.focus == uiChats {
		selected, top = m.searchSelection(selected, top)
	}
	index := top + row
	if m.pinnedCount > 0 {
		if index == 0 {
			return "\x1b[2mPinned\x1b[22m"
		}
		index--
		if index == m.pinnedCount && index < len(m.chats) {
			return "\x1b[2mRecent\x1b[22m"
		}
		if index > m.pinnedCount {
			index--
		}
	}
	if index >= len(m.chats) {
		return ""
	}
	c := m.chats[index]
	name := chatName(c)
	mark := chatMarker(index == selected, c.Unread)
	if m.search != nil && m.focus == uiChats {
		if label, ok := m.search.chatLabels[index]; ok {
			width := m.sidebarWidth()
			if width == 0 {
				width = m.width
			}
			name = m.search.highlight(index, label, width-ansi.StringWidth(mark))
		}
	}
	return mark + name
}

func (m *ui) historyLine(row int) string {
	width := m.historyWidth()
	if m.historyLimit > 0 && len(m.rows) == 0 && len(m.chats) > 0 && row == 0 {
		if m.historyBusy && m.historyErr == nil {
			return ansi.Truncate("Loading messages…", width, "")
		}
		return ansi.Truncate("No messages", width, "")
	}
	selectedRow, top := m.rowIndex, m.rowTop
	if m.focus == uiHistory {
		selectedRow, top = m.searchSelection(selectedRow, top)
	}
	index := top + row
	if index >= len(m.rows) {
		return ""
	}
	line := m.rows[index]
	if line.messageIndex < 0 {
		label := ansi.Truncate(line.text, width, "")
		return strings.Repeat(" ", (width-ansi.StringWidth(label))/2) + "\x1b[2m" + label + "\x1b[22m"
	}
	current := m.rows[selectedRow]
	selected := m.focus != uiChats && line.messageIndex == current.messageIndex && line.attachmentIndex == current.attachmentIndex && line.linkNumber == current.linkNumber
	msg := m.messages[line.messageIndex]
	mark := marker(selected)
	var content string
	if m.search != nil && m.focus == uiHistory && width > len(mark) {
		content = mark + m.search.highlight(index, line.text, width-len(mark))
	} else {
		content = ansi.Truncate(mark+line.text, width, "")
	}
	cells := ansi.StringWidth(content)
	if line.photoRow > 0 {
		// Count placeholder cells without changing the graphics escapes.
		cells = 2 + previewWidth
		a := msg.Attachments[line.attachmentIndex]
		// Keep the layout but omit graphics placeholders behind help.
		if p := m.previews[a.key()]; !m.helpOpen && p != nil && p.rows != nil {
			content = mark + p.rows[line.photoRow-1]
		}
	}
	if msg.FromMe {
		if line.attachmentIndex < 0 && len(content) > len(mark) {
			content = mark + string(m.accent) + content[len(mark):] + "\x1b[39m"
		}
		return strings.Repeat(" ", max(0, width-cells)) + content
	}
	return content
}

func (m *ui) bodyHeight() int {
	return max(0, m.height-1-len(m.footer()))
}

func (m *ui) footer() []string {
	if m.width < 1 || m.height < 2 {
		return nil
	}
	limit := max(1, m.height-2)
	var lines []string
	switch {
	case m.search != nil:
		lines = append(lines, m.search.prompt(m.width))
	case m.focus == uiChats:
		lines = append(lines, "? Help   m Mark Read  h Hide  H Hidden  n New  Enter Open  / Search  p Pin/Unpin  ↑↓ Select  q Quit")
	case m.focus == uiHidden:
		keys := "H/Esc Back"
		if len(m.hidden.chats) > 0 {
			keys = "h Unhide   " + keys + "   / Search   ↑↓ Select"
		}
		lines = append(lines, "? Help   "+keys)
	case m.focus == uiHistory:
		keys := "/ Search   R Reply   c Copy   r React   i Compose   Esc Chats"
		if len(m.rows) > 0 {
			item := m.messages[m.rows[m.rowIndex].messageIndex]
			if item.Editable && item.withinEditWindow() {
				keys = "e Edit   " + keys
			}
		}
		if m.moreHistory {
			keys = "o Older   " + keys
		}
		a, link := m.selectedAttachment(), m.selectedLink()
		if a.isPhoto() {
			keys = "Enter Photo   " + keys
		} else if a.isVideo() && !m.videoActive {
			keys = "Enter Play   " + keys
		} else if link != "" {
			keys = "Enter Open Link   Shift+Enter Copy Link   " + keys
		}
		if m.atLatestMessage() && m.chats[m.chatIndex].Unread {
			key := "m"
			if !a.isPhoto() && !a.isVideo() && link == "" && m.rows[m.rowIndex].attachmentIndex < 0 {
				key = "Enter"
			}
			keys = key + " Mark Read   " + keys
		}
		lines = append(lines, "? Help   "+keys)
	case m.focus == uiCompose || m.focus == uiEdit:
		return m.composeFooter(limit)
	case m.focus == uiReact:
		remove := "No reaction to remove"
		if m.target.MyReaction != "" {
			remove = "7 Remove reaction"
		}
		lines = strings.Split(ansi.Wrap("? Help  "+remove+"  1 Love  2 Like  3 Dislike  4 Laugh  5 Emphasize  6 Question  Esc Back", m.width, ""), "\n")
	}
	if status := m.status(); status != "" {
		lines = append(lines[:min(len(lines), limit-1)], status)
	}
	return lines[:min(len(lines), limit)]
}

func (m *ui) composeFooter(limit int) []string {
	draft := m.drafts[m.composeDraft]
	if m.focus == uiEdit {
		draft = &m.editor.draft
	}
	status := m.status()
	if limit == 1 {
		status = ""
	} else if status != "" {
		limit--
	}
	var lines []string
	if limit > 1 && m.focus == uiCompose {
		if m.composeDraft.guid != "" {
			lines = append(lines, "Reply to "+m.replyContext)
		} else if draft.file != nil {
			lines = append(lines, "Ctrl+X Remove: "+oneLine(draft.file.Name))
		}
	}
	prefix := "> "
	if m.focus == uiEdit {
		prefix = "Edit> "
	} else if draft.file != nil {
		prefix = "Caption> "
	}
	if m.width <= len(prefix) {
		prefix = ""
	}
	const maxDraftLines = 5
	draftHeight := min(maxDraftLines, max(1, limit-len(lines)-1))
	lines = append(lines, draft.visibleLines(prefix, m.width, draftHeight)...)
	if len(lines) < limit {
		keys := "Enter Send   Shift+Enter New Line   Esc Back   Ctrl+V Paste"
		if m.focus == uiEdit {
			keys = "Enter Save   Shift+Enter New Line   Esc Cancel"
		} else if m.composeDraft.guid == "" {
			keys += "   Ctrl+O Attach"
		}
		help := strings.Split(ansi.Wrap(keys, m.width, ""), "\n")
		lines = append(lines, help[:min(len(help), limit-len(lines))]...)
	}
	if status != "" {
		lines = append(lines, status)
	}
	return lines
}

func (d *messageDraft) visibleLines(prefix string, width, limit int) []string {
	before := prefix + cleanText(string(d.text[:d.cursor])) + "▏"
	lines := strings.Split(ansi.Hardwrap(before, width, true), "\n")
	cursorRow := len(lines) - 1
	tail := ansi.Hardwrap(lines[cursorRow]+cleanText(string(d.text[d.cursor:])), width, true)
	lines = append(lines[:cursorRow], strings.Split(tail, "\n")...)
	top := scrollTop(0, cursorRow, limit, len(lines))
	return lines[top:min(top+limit, len(lines))]
}

func (m *ui) status() string {
	switch {
	case m.actionErr != nil:
		return "Action: " + oneLine(m.actionErr.Error())
	case m.focus == uiHidden:
		return ""
	case m.upload != nil:
		if m.upload.size > 0 && m.upload.sent >= m.upload.size {
			return "Finishing upload. Waiting for send confirmation…"
		}
		return fmt.Sprintf("Uploading: %d / %d bytes", m.upload.sent, m.upload.size)
	case m.focus == uiCompose && m.drafts[m.composeDraft].inputErr != nil:
		return "Action: " + oneLine(m.drafts[m.composeDraft].inputErr.Error())
	case m.focus == uiCompose && m.drafts[m.composeDraft].input != "":
		return m.drafts[m.composeDraft].input
	case m.chatsErr != nil:
		return "Chats: " + oneLine(m.chatsErr.Error())
	case m.historyErr != nil:
		return "History: " + oneLine(m.historyErr.Error())
	case m.viewErr != nil && m.focus == uiHistory:
		return "Open chat: " + oneLine(m.viewErr.Error())
	case m.focus == uiHistory && len(m.chats) > 0 &&
		(m.chats[m.chatIndex].ID == m.viewPending || m.chats[m.chatIndex].ID == m.viewing):
		return "Waiting for Mac to mark chat read…"
	case m.historyBusy && m.moreHistory && m.historyLimit > len(m.messages):
		return "Loading older messages…"
	case m.writing:
		return "Working…"
	case m.reconnecting && m.watchErr != nil:
		return "Reconnecting: " + oneLine(m.watchErr.Error())
	case m.reconnecting:
		return "Reconnecting…"
	case m.videoActive:
		return "Video active…"
	}
	return ""
}

func marker(selected bool) string {
	if selected {
		return "> "
	}
	return "  "
}

func chatMarker(selected, unread bool) string {
	if unread {
		return marker(selected) + "• "
	}
	return marker(selected) + "  "
}

func pad(s string, width int) string {
	s = ansi.Truncate(s, width, "")
	return s + strings.Repeat(" ", width-ansi.StringWidth(s))
}

func chatName(c chat) string {
	if name := oneLine(c.ContactName); name != "" {
		return name
	}
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
