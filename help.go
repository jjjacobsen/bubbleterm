package main

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

const helpText = `BubbleTerm controls
↑/↓ or k/j  Scroll help   Esc or ?  Close help

GENERAL
?  Open help outside text input
Esc  Dismiss a footer error without leaving the pane
F2  Show full details for the latest error, even after dismissal
q  Quit from chats, hidden chats, history, or reactions
Ctrl+C  Quit from any mode
Ctrl+R  Refresh from chats, history, or reactions

NAVIGATION (CHATS, HIDDEN CHATS, HISTORY, HELP)
↑/↓ or k/j  Move up/down
PgUp/PgDn  Move one page
Ctrl+U/Ctrl+D  Move half a page
Home/End or </>  Move to first/last

CHATS
Enter, Tab, or →  Open history
J/K  Select next/previous chat in the chat list (also from history)
n/N  Start a new chat
p/P  Pin or unpin chat
m/M  Mark selected chat read (or opened chat from history)
h  Hide chat
H  Show hidden chats

HIDDEN CHATS
J/K  Also move down/up
h  Unhide chat
H or Esc  Return to chats

HISTORY
i  Compose (also in chats)
R  Reply in a thread
r  Open reactions
e  Edit an eligible sent iMessage
c/C  Copy message
Enter  Open photo, video, or link, or mark latest read
Shift+Enter  Copy link
o  Load older messages
Tab, Esc, or ←  Return to chats

REACTIONS
1 Love, 2 Like, 3 Dislike, 4 Laugh
5 Emphasize, 6 Question, 7 Remove your reaction
Esc  Return to history

COMPOSE, REPLY, AND EDIT
Enter  Send message or save edit
Shift+Enter  Insert a new line
Esc or Tab  Leave draft or cancel edit
Ctrl+V  Paste text or a photo while composing
Ctrl+O  Attach a photo or video to a new message
Ctrl+X  Remove attachment

SEARCH
/  Search chats, hidden chats, or loaded history
Ctrl+J/Ctrl+K  Select next/previous match
Enter  Jump to match (chat list: select without opening)
Esc  Cancel search

NEW CHAT
Type a name, phone number, or email
Tab/↓ or Shift+Tab/↑  Select next/previous suggestion
Enter  Add recipient, open chat, or start first message
Backspace in empty query  Remove last recipient
Ctrl+R  Retry if loading recipients failed
Enter in first message  Send
Shift+Enter or Ctrl+J in first message  New line
Tab in first message  Return to recipients
Esc  Cancel

TEXT INPUT
←/→  Move cursor
Home/End  Move to start/end
Backspace/Delete  Delete before/after cursor
Type or paste text, including ?`

func (m *ui) overlayPage() (lines []string, width, height int, framed bool) {
	width, height = max(1, m.width), max(1, m.height)
	framed = width >= 32 && height >= 7
	if framed {
		width, height = min(76, width-8)-4, min(26, height-2)-2
	}
	text := ansi.Wrap(m.overlay, width, "")
	lines = strings.Split(ansi.Hardwrap(text, width, true), "\n")
	return lines, width, min(height, len(lines)), framed
}

func (m *ui) overlayKey(key string) {
	if key == "esc" || key == "?" {
		m.overlay = ""
		return
	}
	lines, _, height, _ := m.overlayPage()
	last := len(lines) - height
	m.overlayTop = min(last, moveIndex(key, min(m.overlayTop, last), len(lines), height))
}

func (m *ui) overlayView() string {
	lines, width, height, framed := m.overlayPage()
	top := min(m.overlayTop, len(lines)-height)
	lines = lines[top : top+height]
	if !framed {
		return strings.Join(lines, "\n")
	}
	border := strings.Repeat("─", width+2)
	box := []string{"┌" + border + "┐"}
	for _, line := range lines {
		box = append(box, "│ "+pad(line, width)+" │")
	}
	box = append(box, "└"+border+"┘")

	background := strings.Split(m.mainView(), "\n")
	left, topRow := (m.width-width-4)/2, (m.height-height-2)/2
	for i, line := range box {
		row := topRow + i
		background[row] = overlayLine(background[row], line, left, left+width+4)
	}
	return strings.Join(background, "\n")
}

func overlayLine(background, line string, left, right int) string {
	tail := ""
	if width := ansi.StringWidth(background) - right; width > 0 {
		tail = ansi.TruncateLeft(background, right, "")
		// TruncateLeft keeps a grapheme that straddles the edge. Blank its exposed cells.
		if extra := ansi.StringWidth(tail) - width; extra > 0 {
			tail = ansi.TruncateLeft(tail, extra, "")
			tail = strings.Repeat(" ", width-ansi.StringWidth(tail)) + tail
		}
	}
	return pad(background, left) + "\x1b[0m" + line + tail
}
