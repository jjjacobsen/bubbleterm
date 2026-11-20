package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

const historyDetailsQuery = `
WITH targets AS (
    SELECT ROWID AS id, lower(guid) AS guid,
           CASE
               WHEN item_type = 3 AND group_action_type = 0 THEN '[Left the group]'
               WHEN item_type IN (1, 2, 3) THEN '[Group event]'
           END AS notice
    FROM message WHERE ROWID IN (%s)
), events AS (
    SELECT ROWID AS event_id, date, associated_message_type AS kind,
           associated_message_emoji AS emoji, is_from_me AS from_me,
           CASE WHEN is_from_me = 1 THEN 0 ELSE handle_id END AS sender,
           CASE WHEN associated_message_guid GLOB 'p:*/*'
                THEN substr(associated_message_guid, 3, instr(associated_message_guid, '/') - 3)
                ELSE '0' END AS part,
           lower(CASE WHEN associated_message_guid GLOB 'p:*/*'
                      THEN substr(associated_message_guid, instr(associated_message_guid, '/') + 1)
                      WHEN associated_message_guid GLOB 'bp:*' THEN substr(associated_message_guid, 4)
                      ELSE associated_message_guid END) AS guid
    FROM message
    WHERE associated_message_guid IS NOT NULL AND COALESCE(error, 0) = 0
      AND (associated_message_type BETWEEN 2000 AND 2007 OR associated_message_type BETWEEN 3000 AND 3007)
)
SELECT targets.id, part, sender, from_me, kind, emoji, NULL AS notice, date, event_id
FROM events JOIN targets USING (guid)
UNION ALL
SELECT id, NULL, NULL, NULL, NULL, NULL, notice, NULL, NULL
FROM targets WHERE notice IS NOT NULL
ORDER BY date, event_id`

type reactionKey struct {
	Part   string `json:"part"`
	Sender int    `json:"sender"`
	FromMe int    `json:"from_me"`
}

type reactionEvent struct {
	reactionKey
	ID     int    `json:"id"`
	Kind   int    `json:"kind"`
	Emoji  string `json:"emoji"`
	Notice string `json:"notice"`
}

func (r reactionEvent) symbol() string {
	if r.Kind == 2006 {
		return r.Emoji
	}
	return [...]string{"❤️", "👍", "👎", "😂", "‼️", "❓", "", "🏷️"}[r.Kind-2000]
}

func (b backend) loadHistoryDetails(rows []message) error {
	if len(rows) == 0 {
		return nil
	}
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = strconv.Itoa(row.ID)
	}
	// Match by target GUID, not chat membership: synced reactions can be in a different chat.
	// imsg does not distinguish body parts or replace different reactions from the same sender.
	query := fmt.Sprintf(historyDetailsQuery, strings.Join(ids, ","))
	out, err := b.run("read history details", `exec /usr/bin/sqlite3 -batch -bail -readonly -init /dev/null -json "$HOME/Library/Messages/chat.db" `+shellQuote(query))
	if err != nil || len(out) == 0 {
		return err
	}
	var events []reactionEvent
	if err := json.Unmarshal(out, &events); err != nil {
		return fmt.Errorf("decode history details: %w", err)
	}
	applyHistoryDetails(rows, events)
	return nil
}

func applyHistoryDetails(rows []message, events []reactionEvent) {
	active := make(map[int]map[reactionKey]reactionEvent)
	notices := make(map[int]string)
	for _, event := range events {
		if event.Notice != "" {
			notices[event.ID] = event.Notice
			continue
		}
		reactions := active[event.ID]
		key := event.reactionKey
		if event.Kind >= 3000 {
			previous := reactions[key]
			if event.Kind == previous.Kind+1000 && (event.Kind != 3006 || event.Emoji == "" || event.Emoji == previous.Emoji) {
				delete(reactions, key)
			}
			continue
		}
		if reactions == nil {
			reactions = make(map[reactionKey]reactionEvent)
			active[event.ID] = reactions
		}
		reactions[key] = event
	}
	for i := range rows {
		row := &rows[i]
		if row.Text == "" && len(row.Attachments) == 0 {
			row.Text = notices[row.ID]
		}
		mine := make(map[string]string)
		others := make(map[string]int)
		for key, event := range active[row.ID] {
			if symbol := event.symbol(); symbol != "" {
				if key.FromMe == 1 {
					mine[key.Part] = symbol
				} else {
					others[symbol]++
				}
			}
		}
		var symbols []string
		for _, part := range slices.Sorted(maps.Keys(mine)) {
			symbols = append(symbols, mine[part])
		}
		row.MyReaction = strings.Join(symbols, " ")
		symbols = symbols[:0]
		for _, symbol := range slices.Sorted(maps.Keys(others)) {
			if count := others[symbol]; count > 1 {
				symbol += "×" + strconv.Itoa(count)
			}
			symbols = append(symbols, symbol)
		}
		row.OtherReactions = strings.Join(symbols, " ")
	}
}
