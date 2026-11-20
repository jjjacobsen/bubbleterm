package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

const myReactionQuery = `
WITH events AS (
    SELECT ROWID AS event_id, date, associated_message_type AS kind,
           associated_message_emoji AS emoji,
           CASE WHEN associated_message_guid GLOB 'p:*/*'
                THEN substr(associated_message_guid, 3, instr(associated_message_guid, '/') - 3)
                ELSE '0' END AS part,
           lower(CASE WHEN associated_message_guid GLOB 'p:*/*'
                      THEN substr(associated_message_guid, instr(associated_message_guid, '/') + 1)
                      WHEN associated_message_guid GLOB 'bp:*' THEN substr(associated_message_guid, 4)
                      ELSE associated_message_guid END) AS guid
    FROM message
    WHERE associated_message_guid IS NOT NULL AND is_from_me = 1 AND COALESCE(error, 0) = 0
      AND (associated_message_type BETWEEN 2000 AND 2007 OR associated_message_type BETWEEN 3000 AND 3007)
)
SELECT guid, part, kind, emoji FROM events
WHERE guid IN (SELECT lower(guid) FROM message WHERE ROWID IN (%s))
ORDER BY date, event_id`

type reactionEvent struct {
	GUID  string `json:"guid"`
	Part  string `json:"part"`
	Kind  int    `json:"kind"`
	Emoji string `json:"emoji"`
}

func (r reactionEvent) symbol() string {
	if r.Kind == 2006 {
		return r.Emoji
	}
	return [...]string{"❤️", "👍", "👎", "😂", "‼️", "❓", "", "🏷️"}[r.Kind-2000]
}

func (b backend) loadMyReactions(rows []message) error {
	if len(rows) == 0 {
		return nil
	}
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = strconv.Itoa(row.ID)
	}
	// Match by target GUID, not chat membership: synced reactions can be in a different chat.
	// imsg history omits bp: targets and does not distinguish body parts or replacements.
	query := fmt.Sprintf(myReactionQuery, strings.Join(ids, ","))
	out, err := b.run("read Tapbacks", `exec /usr/bin/sqlite3 -batch -bail -readonly -init /dev/null -json "$HOME/Library/Messages/chat.db" `+shellQuote(query))
	if err != nil || len(out) == 0 {
		return err
	}
	var events []reactionEvent
	if err := json.Unmarshal(out, &events); err != nil {
		return fmt.Errorf("decode Tapbacks: %w", err)
	}
	active := make(map[string]map[string]reactionEvent)
	for _, event := range events {
		parts := active[event.GUID]
		if event.Kind >= 3000 {
			previous := parts[event.Part]
			if event.Kind == previous.Kind+1000 && (event.Emoji == "" || event.Emoji == previous.Emoji) {
				delete(parts, event.Part)
			}
			continue
		}
		if parts == nil {
			parts = make(map[string]reactionEvent)
			active[event.GUID] = parts
		}
		parts[event.Part] = event
	}
	for i := range rows {
		parts := active[strings.ToLower(rows[i].GUID)]
		var symbols []string
		for _, part := range slices.Sorted(maps.Keys(parts)) {
			if symbol := parts[part].symbol(); symbol != "" {
				symbols = append(symbols, symbol)
			}
		}
		rows[i].MyReaction = strings.Join(symbols, " ")
	}
	return nil
}
