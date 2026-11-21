package main

import (
	"context"
	"encoding/json"
	"html"
	"os/exec"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

type desktopNotifications struct {
	lastID int64
}

type watchEvent struct {
	ID          int64        `json:"id"`
	CreatedAt   time.Time    `json:"created_at"`
	FromMe      *bool        `json:"is_from_me"`
	Type        string       `json:"type"`
	IsReaction  bool         `json:"is_reaction"`
	UnsentAt    string       `json:"unsent_at"`
	Sender      string       `json:"sender"`
	SenderName  string       `json:"sender_name"`
	IsGroup     bool         `json:"is_group"`
	ChatName    string       `json:"chat_name"`
	Text        string       `json:"text"`
	Attachments []attachment `json:"attachments"`
}

func (n *desktopNotifications) onWatch() func(context.Context, json.RawMessage) {
	command, err := exec.LookPath("notify-send")
	if err != nil {
		return nil
	}
	// imsg starts at the current tail. Also skip old texts newly synced into it.
	startedAt := time.Now().Truncate(time.Millisecond)
	return func(ctx context.Context, row json.RawMessage) {
		var event watchEvent
		if err := json.Unmarshal(row, &event); err != nil ||
			event.FromMe == nil || *event.FromMe || event.Type != "" || event.IsReaction || event.UnsentAt != "" ||
			event.ID <= n.lastID || event.CreatedAt.Before(startedAt) {
			return
		}
		summary, body := event.notification()
		if body == "" {
			return
		}
		// New rows arrive in ID order. Keep this cursor across watch reconnects.
		n.lastID = event.ID
		go sendNotification(ctx, command, summary, body)
	}
}

func (event watchEvent) notification() (string, string) {
	body := notificationText(event.Text, 100)
	if body == "" && slices.ContainsFunc(event.Attachments, func(a attachment) bool { return !a.isPreviewPayload() }) {
		body = "Attachment"
	}
	summary := notificationText(event.SenderName, 48)
	if summary == "" {
		summary = notificationText(event.Sender, 48)
	}
	if summary == "" {
		summary = "New message"
	}
	if event.IsGroup {
		group := notificationText(event.ChatName, 48)
		if group == "" {
			group = "Group chat"
		}
		summary += " · " + group
	}
	return summary, body
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
