package main

import (
	"context"
	"html"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

type watchEvent struct {
	ID             int          `json:"id"`
	ChatID         int          `json:"chat_id"`
	CreatedAt      time.Time    `json:"created_at"`
	FromMe         *bool        `json:"is_from_me"`
	Type           string       `json:"type"`
	IsReaction     bool         `json:"is_reaction"`
	UnsentAt       string       `json:"unsent_at"`
	Sender         string       `json:"sender"`
	SenderName     string       `json:"sender_name"`
	IsGroup        bool         `json:"is_group"`
	ChatName       *string      `json:"chat_name"`
	ChatIdentifier string       `json:"chat_identifier"`
	ChatGUID       string       `json:"chat_guid"`
	Text           string       `json:"text"`
	Attachments    []attachment `json:"attachments"`
}

func (event watchEvent) newMessage(startedAt time.Time, lastID int) bool {
	// Skip old texts newly synced into the watch stream.
	return event.ChatID != 0 && event.ID > lastID && !event.CreatedAt.Before(startedAt) &&
		event.FromMe != nil && event.Type == "" && !event.IsReaction && event.UnsentAt == ""
}

// Names stay in memory and are shared by watch reconnects on the same SSH session.
type notificationDirectory struct {
	mu      sync.Mutex
	chats   map[int]chat
	names   map[string]string
	expires time.Time
	loading chan struct{}
}

func (d *notificationDirectory) updateChats(chats []chat) {
	byID := make(map[int]chat, len(chats))
	for _, c := range chats {
		c.IsGroup = c.IsGroup || strings.Contains(c.Identifier, ";+;")
		byID[c.ID] = c
	}
	d.mu.Lock()
	d.chats = byID
	d.mu.Unlock()
}

func (d *notificationDirectory) refresh(load func() (map[string]string, error)) <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.loading == nil && !time.Now().Before(d.expires) {
		done := make(chan struct{})
		d.loading = done
		go func() {
			names, _ := load()
			d.mu.Lock()
			d.names = names
			d.expires = time.Now().Add(time.Minute)
			d.loading = nil
			close(done)
			d.mu.Unlock()
		}()
	}
	return d.loading
}

func (b backend) notificationHandler(command string, hidden *chatStore) func(context.Context, watchEvent) {
	if command == "" {
		return nil
	}
	s, err := b.session()
	if err != nil {
		return nil
	}
	d := &s.notificationDirectory
	d.refresh(b.directoryContacts)
	return func(ctx context.Context, event watchEvent) {
		if hidden.containsID(event.ChatID) {
			return
		}
		body := event.notificationBody()
		if body == "" {
			return
		}
		pending := d.refresh(b.directoryContacts)
		go func() {
			summary := d.summary(ctx, event, pending)
			// The chat may have been hidden while its contact name was loading.
			if ctx.Err() == nil && !hidden.containsID(event.ChatID) {
				sendNotification(ctx, command, summary, body)
			}
		}()
	}
}

func (d *notificationDirectory) lookup(event watchEvent, address string) (chat, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.chats[event.ChatID]
	if address != "" && !event.group(c) && c.ContactName != "" {
		if identifier, _ := normalizeAddress(c.Identifier); identifier == address {
			return c, c.ContactName
		}
	}
	return c, d.names[address]
}

func (d *notificationDirectory) summary(ctx context.Context, event watchEvent, pending <-chan struct{}) string {
	address, _ := normalizeAddress(event.Sender)
	c, name := d.lookup(event, address)
	summary := notificationText(event.SenderName, 48)
	handle := notificationText(event.Sender, 48)
	if summary == "" || summary == handle {
		if name == "" && address != "" && pending != nil {
			// Only the notification can wait for a cold catalog. Watch and UI refresh keep running.
			wait, cancel := context.WithTimeout(ctx, 2*time.Second)
			select {
			case <-pending:
			case <-wait.Done():
			}
			cancel()
			c, name = d.lookup(event, address)
		}
		summary = notificationText(name, 48)
	}
	if summary == "" {
		summary = handle
	}
	if summary == "" {
		summary = "New message"
	}
	if event.group(c) {
		var group string
		if event.ChatName != nil {
			group = groupNotificationName(*event.ChatName, event.ChatIdentifier, event.ChatGUID, c.Identifier)
		} else if c.IsGroup {
			group = groupNotificationName(c.Name, event.ChatIdentifier, event.ChatGUID, c.Identifier)
		}
		if group == "" {
			group = "Group chat"
		}
		summary += " · " + group
	}
	return summary
}

func (event watchEvent) group(c chat) bool {
	return event.IsGroup || c.IsGroup || strings.Contains(event.ChatGUID, ";+;") || strings.Contains(event.ChatIdentifier, ";+;")
}

func groupNotificationName(name string, identifiers ...string) string {
	name = strings.TrimSpace(name)
	// imsg can emit the raw chat identifier when display_name is NULL.
	if strings.Contains(name, ";+;") {
		return ""
	}
	for _, identifier := range identifiers {
		if _, suffix, ok := strings.Cut(identifier, ";+;"); ok {
			identifier = suffix
		}
		if name == strings.TrimSpace(identifier) {
			return ""
		}
	}
	return notificationText(name, 48)
}

func (event watchEvent) notificationBody() string {
	body := notificationText(event.Text, 100)
	if body == "" && slices.ContainsFunc(event.Attachments, func(a attachment) bool { return !a.isPreviewPayload() }) {
		return "Attachment"
	}
	return body
}

func notificationText(text string, limit int) string {
	text = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r), r == '\ufffc':
			return ' '
		case unicode.Is(unicode.Bidi_Control, r):
			return -1
		default:
			return r
		}
	}, ansi.Strip(text))
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return text
}

func sendNotification(ctx context.Context, command, summary, body string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Send only to the local desktop. The body supports markup, the title does not.
	_ = exec.CommandContext(ctx, command, "--app-name=Bubbleterm", "--transient", "--", summary, html.EscapeString(body)).Run()
}
