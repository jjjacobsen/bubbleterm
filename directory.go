package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"slices"
	"strings"
	"unicode"
)

type recipientOption struct {
	Name    string
	Address string
	Chat    *chat
}

type directory struct {
	options []recipientOption
	chats   []directoryChat
}

type directoryChat struct {
	chat
	addresses []string
}

// imsg chats only lists conversations with messages, even with a high limit.
// Read the same chat and handle tables directly to include every conversation.
const directoryChatsQuery = `
SELECT c.ROWID AS id, coalesce(c.chat_identifier, '') AS identifier,
       trim(coalesce(c.display_name, '')) AS display_name,
       (c.style = 43 OR instr(c.guid, ';+;') > 0
        OR instr(coalesce(c.chat_identifier, ''), ';+;') > 0) AS is_group,
       json_group_array(h.id) FILTER (WHERE j.chat_id IS NOT NULL) AS members
FROM chat c
LEFT JOIN chat_handle_join j ON j.chat_id = c.ROWID
LEFT JOIN handle h ON h.ROWID = j.handle_id
GROUP BY c.ROWID
ORDER BY c.ROWID DESC`

// Use the native Address Book stores where SSH already has read access. Do not
// start Contacts or ask macOS to authorize a new process to use Contacts APIs.
const directoryContactsQuery = `
WITH addresses AS (
  SELECT ZOWNER AS owner, ZFULLNUMBER AS address FROM ZABCDPHONENUMBER
  UNION ALL
  SELECT ZOWNER, ZADDRESS FROM ZABCDEMAILADDRESS
)
SELECT coalesce(nullif(trim(coalesce(r.ZFIRSTNAME, '') || ' ' ||
                            coalesce(r.ZMIDDLENAME || ' ', '') ||
                            coalesce(r.ZLASTNAME, '')), ''),
                nullif(r.ZNICKNAME, ''), nullif(r.ZORGANIZATION, ''), r.ZNAME, '') AS name,
       a.address
FROM addresses a JOIN ZABCDRECORD r ON r.Z_PK = a.owner
WHERE a.address IS NOT NULL
ORDER BY r.Z_PK`

func (b backend) directory() (directory, error) {
	out, err := b.read("read recipient chats", queryMessagesDB+shellQuote(directoryChatsQuery))
	if err != nil {
		return directory{}, err
	}
	var rows []struct {
		ID          int    `json:"id"`
		Identifier  string `json:"identifier"`
		DisplayName string `json:"display_name"`
		IsGroup     int    `json:"is_group"`
		Members     string `json:"members"`
	}
	if len(out) > 0 {
		if err := json.Unmarshal(out, &rows); err != nil {
			return directory{}, fmt.Errorf("decode recipient chats: %w", err)
		}
	}
	names, err := b.directoryContacts()
	if err != nil {
		return directory{}, err
	}
	d := directory{chats: make([]directoryChat, 0, len(rows))}
	for _, row := range rows {
		c := chat{ID: row.ID, Identifier: row.Identifier, Name: row.DisplayName, IsGroup: row.IsGroup != 0}
		named := c.Name != "" && c.Name != c.Identifier
		if !named {
			c.Name = c.Identifier
		}
		var members []string
		if err := json.Unmarshal([]byte(row.Members), &members); err != nil {
			return directory{}, fmt.Errorf("decode chat participants: %w", err)
		}
		if !c.IsGroup && len(members) == 0 {
			members = []string{c.Identifier}
		}
		var addresses []string
		for _, member := range members {
			address, err := normalizeAddress(member)
			if err != nil {
				continue
			}
			addresses = append(addresses, address)
			name := names[address]
			if !c.IsGroup && len(members) == 1 {
				if name == "" && named {
					name = c.Name
				}
				c.ContactName = name
			}
			names[address] = name
		}
		// Do not match a subset of a chat if any stored handle is not sendable.
		if len(addresses) != len(members) {
			addresses = nil
		}
		d.chats = append(d.chats, directoryChat{chat: c, addresses: sortedRecipients(addresses)})
		if named {
			d.options = append(d.options, recipientOption{Name: c.Name, Chat: &d.chats[len(d.chats)-1].chat})
		}
	}
	for address, name := range names {
		if name == "" {
			name = address
		}
		d.options = append(d.options, recipientOption{Name: name, Address: address})
	}
	slices.SortStableFunc(d.options, func(a, b recipientOption) int {
		if order := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); order != 0 {
			return order
		}
		return strings.Compare(a.Address, b.Address)
	})
	return d, nil
}

func (b backend) directoryContacts() (map[string]string, error) {
	command := `for db in "$HOME/Library/Application Support/AddressBook"/AddressBook-v*.abcddb "$HOME/Library/Application Support/AddressBook"/Sources/*/AddressBook-v*.abcddb; do
  [ -r "$db" ] || continue
  /usr/bin/sqlite3 -batch -bail -readonly -init /dev/null -json "$db" ` + shellQuote(directoryContactsQuery) + ` 2>/dev/null || continue
done`
	out, err := b.read("read local contacts", "/bin/sh -c "+shellQuote(command))
	if err != nil {
		return nil, err
	}
	names := make(map[string]string)
	d := json.NewDecoder(bytes.NewReader(out))
	for {
		var rows []recipientOption
		if err := d.Decode(&rows); err == io.EOF {
			return names, nil
		} else if err != nil {
			return nil, fmt.Errorf("decode local contacts: %w", err)
		}
		for _, row := range rows {
			address, err := normalizeAddress(row.Address)
			if err == nil && names[address] == "" {
				names[address] = strings.TrimSpace(row.Name)
			}
		}
	}
}

func (d directory) suggestions(query string) []recipientOption {
	query = strings.ToLower(strings.TrimSpace(query))
	digits, international := phoneDigits(query)
	var options []recipientOption
	for _, option := range d.options {
		if strings.Contains(strings.ToLower(option.Name), query) || strings.Contains(option.Address, query) ||
			digits != "" && strings.HasPrefix(option.Address, "+") &&
				(!international && strings.Contains(option.Address, digits) || international && strings.HasPrefix(option.Address, "+"+digits)) {
			options = append(options, option)
		}
	}
	return options
}

func (d directory) match(addresses []string) (chat, bool, error) {
	want, err := recipientSet(addresses)
	if err != nil || len(want) == 0 {
		return chat{}, false, err
	}
	var match chat
	for _, c := range d.chats {
		if slices.Equal(c.addresses, want) {
			if match.ID != 0 {
				return chat{}, false, errors.New("multiple chats have these recipients")
			}
			match = c.chat
		}
	}
	return match, match.ID != 0, nil
}

func recipientSet(addresses []string) ([]string, error) {
	set := make([]string, 0, len(addresses))
	for _, address := range addresses {
		normalized, err := normalizeAddress(address)
		if err != nil {
			return nil, err
		}
		set = append(set, normalized)
	}
	return sortedRecipients(set), nil
}

func sortedRecipients(addresses []string) []string {
	slices.Sort(addresses)
	return slices.Compact(addresses)
}

func normalizeAddress(input string) (string, error) {
	input = strings.TrimSpace(input)
	if strings.Contains(input, "@") {
		if address, err := mail.ParseAddress(input); err == nil && address.Address == input {
			return strings.ToLower(input), nil
		}
	} else {
		digits, international := phoneDigits(input)
		switch {
		case !international && len(digits) == 10:
			return "+1" + digits, nil
		case len(digits) == 11 && digits[0] == '1':
			return "+" + digits, nil
		case international && len(digits) >= 7 && len(digits) <= 15 && digits[0] != '0' && digits[0] != '1':
			return "+" + digits, nil
		}
	}
	return "", errors.New("enter a phone number or email address")
}

func phoneDigits(input string) (string, bool) {
	international := strings.HasPrefix(input, "+")
	var out strings.Builder
	for _, r := range strings.TrimPrefix(input, "+") {
		switch {
		case r >= '0' && r <= '9':
			out.WriteRune(r)
		case unicode.IsSpace(r) || r == '(' || r == ')' || r == '-' || r == '.':
		default:
			return "", international
		}
	}
	return out.String(), international
}
