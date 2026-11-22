package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const editWindow = 15 * time.Minute

func (m message) withinEditWindow() bool {
	age := time.Since(m.CreatedAt)
	return age >= 0 && age < editWindow
}

func (b backend) loadEditEligibility(rows []message) error {
	var ids []string
	for _, row := range rows {
		if row.FromMe && row.withinEditWindow() && row.UnsentAt == nil && row.GUID != "" &&
			strings.TrimSpace(row.Text) != "" && !strings.ContainsAny(row.Text, "\x00\ufffc\ufffd") {
			ids = append(ids, strconv.Itoa(row.ID))
		}
	}
	if len(ids) == 0 {
		return nil
	}
	// imsg history omits the message service. The chat service can differ from it.
	query := `SELECT ROWID AS id FROM message WHERE ROWID IN (` + strings.Join(ids, ",") + `)
AND service = 'iMessage' AND is_from_me = 1 AND is_sent = 1 AND is_finished = 1
AND COALESCE(error, 0) = 0 AND COALESCE(date_retracted, 0) = 0
AND COALESCE(item_type, 0) = 0 AND COALESCE(is_system_message, 0) = 0
AND COALESCE(associated_message_type, 0) = 0 AND balloon_bundle_id IS NULL
AND part_count = 1
AND NOT EXISTS (SELECT 1 FROM message_attachment_join WHERE message_id = message.ROWID)`
	out, err := b.read("check edit eligibility", `exec /usr/bin/sqlite3 -batch -bail -readonly -init /dev/null -json "$HOME/Library/Messages/chat.db" `+shellQuote(query))
	if err != nil || len(out) == 0 {
		return err
	}
	var eligible []struct{ ID int }
	if err := json.Unmarshal(out, &eligible); err != nil {
		return fmt.Errorf("decode edit eligibility: %w", err)
	}
	editable := make(map[int]bool, len(eligible))
	for _, row := range eligible {
		editable[row.ID] = true
	}
	for i := range rows {
		rows[i].Editable = editable[rows[i].ID]
	}
	return nil
}

func (b backend) edit(id int, guid, text string) error {
	return b.run("edit message", fmt.Sprintf(`exec "$HOME/.local/bin/bubbleterm-edit" %d %s %s`, id, shellQuote(guid), shellQuote(text)))
}
