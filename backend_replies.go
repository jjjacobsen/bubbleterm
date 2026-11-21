package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const historyThreadsQuery = `
SELECT reply.ROWID AS id, reply.thread_originator_guid AS guid,
       reply.thread_originator_part AS part, parent.part_count AS parent_parts,
       CASE WHEN parent.is_from_me = 1 THEN 'You' ELSE handle.id END AS author,
       CASE WHEN parent.date_retracted > 0 THEN '[Unsent message]'
            WHEN parent.text <> '' THEN substr(parent.text, 1, 160)
            WHEN parent.cache_has_attachments > 0 THEN '[Attachment]' END AS text,
       parent.date / 1000000000 + 978307200 AS unix_date
FROM message AS reply
LEFT JOIN message AS parent ON parent.guid = reply.thread_originator_guid
LEFT JOIN handle ON handle.ROWID = parent.handle_id
WHERE reply.ROWID IN (%s) AND reply.thread_originator_guid <> ''`

type threadOrigin struct {
	GUID        string `json:"guid"`
	Part        string `json:"part"`
	ParentParts int    `json:"parent_parts"`
	Author      string `json:"author"`
	Text        string `json:"text"`
	UnixDate    int64  `json:"unix_date"`
}

func (t threadOrigin) canContinue() bool {
	// The GUID-only Mac helper can target only the parent's final part.
	if t.ParentParts == 0 || t.Part == "" {
		return true
	}
	last := strconv.Itoa(t.ParentParts - 1)
	return strings.HasPrefix(t.Part, last+":"+last+":")
}

func (b backend) loadHistoryThreads(rows []message) error {
	if len(rows) == 0 {
		return nil
	}
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = strconv.Itoa(row.ID)
	}
	query := fmt.Sprintf(historyThreadsQuery, strings.Join(ids, ","))
	out, err := b.run("read reply threads", `exec /usr/bin/sqlite3 -batch -bail -readonly -init /dev/null -json "$HOME/Library/Messages/chat.db" `+shellQuote(query))
	if err != nil || len(out) == 0 {
		return err
	}
	var threads []struct {
		ID int `json:"id"`
		threadOrigin
	}
	if err := json.Unmarshal(out, &threads); err != nil {
		return fmt.Errorf("decode reply threads: %w", err)
	}
	byID := make(map[int]threadOrigin, len(threads))
	for _, thread := range threads {
		byID[thread.ID] = thread.threadOrigin
	}
	for i := range rows {
		rows[i].Thread = byID[rows[i].ID]
	}
	return nil
}

func (b backend) reply(id int, guid, text string) error {
	command := fmt.Sprintf(`exec "$HOME/.local/bin/bubbleterm-reply" %d %s %s`, id, shellQuote(guid), shellQuote(text))
	_, err := b.run("send reply", command)
	return err
}
