package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var errNewChatUncertain = errors.New("send outcome unknown. Check Messages before retrying")

func (b backend) startChat(addresses []string, text string) (chat, error) {
	if strings.TrimSpace(text) == "" || strings.ContainsRune(text, '\x00') {
		return chat{}, errors.New("enter a non-empty text message without NUL bytes")
	}
	addresses, err := recipientSet(addresses)
	if err != nil {
		return chat{}, err
	}
	if len(addresses) == 0 {
		return chat{}, errors.New("enter at least one recipient")
	}

	d, err := b.directory()
	if err != nil {
		return chat{}, err
	}
	target, found, err := d.match(addresses)
	if err != nil {
		return chat{}, err
	}
	body := " " + shellQuote("--text="+text)
	if found {
		_, err := b.newChatSend(fmt.Sprintf("send --chat-id %d", target.ID) + body)
		return target, err
	}

	var guid string
	if len(addresses) > 1 {
		guid, err = b.newGroupSend(addresses, text)
	} else {
		guid, err = b.newChatSend("send --no-sms-fallback " + shellQuote("--to="+addresses[0]) + body)
	}
	if err != nil {
		return chat{}, err
	}
	target, err = b.newChatReceipt(addresses, guid)
	if err != nil {
		return chat{}, fmt.Errorf("%w: %w", errNewChatUncertain, err)
	}
	return target, nil
}

func (b backend) newGroupSend(addresses []string, text string) (string, error) {
	command := `exec "$HOME/.local/bin/bubbleterm-group" ` + shellQuote(text)
	for _, address := range addresses {
		command += " " + shellQuote(address)
	}
	var out bytes.Buffer
	if err := b.runTo("send first group message", command, &out, 45*time.Second); err != nil {
		return "", fmt.Errorf("%w: %w", errNewChatUncertain, err)
	}
	var receipt struct {
		Status string `json:"status"`
		GUID   string `json:"guid"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &receipt); err != nil {
		return "", fmt.Errorf("%w: decode group send receipt: %w", errNewChatUncertain, err)
	}
	switch {
	case receipt.Status == "sent" && receipt.GUID != "" && receipt.Error == "":
		return receipt.GUID, nil
	case receipt.Status == "not_sent" && receipt.GUID == "" && receipt.Error != "":
		return "", errors.New(receipt.Error)
	case receipt.Status == "unknown" && receipt.Error != "":
		return "", fmt.Errorf("%w: %s", errNewChatUncertain, receipt.Error)
	default:
		return "", fmt.Errorf("%w: invalid group send receipt", errNewChatUncertain)
	}
}

func (b backend) newChatSend(command string) (string, error) {
	var out bytes.Buffer
	if err := b.runTo("send first message", imsg+command+" --json", &out, 45*time.Second); err != nil {
		// The SSH channel or imsg can fail after Messages accepts the send.
		return "", fmt.Errorf("%w: %w", errNewChatUncertain, err)
	}
	var receipt struct {
		Status      string `json:"status"`
		GUID        string `json:"guid"`
		MessageGUID string `json:"messageGuid"`
	}
	if err := json.Unmarshal(out.Bytes(), &receipt); err != nil {
		return "", fmt.Errorf("%w: decode send receipt: %w", errNewChatUncertain, err)
	}
	if receipt.MessageGUID != "" {
		return receipt.MessageGUID, nil
	}
	if receipt.Status != "sent" || receipt.GUID == "" {
		return "", fmt.Errorf("%w: no message ID in send receipt", errNewChatUncertain)
	}
	return receipt.GUID, nil
}

func (b backend) newChatReceipt(addresses []string, guid string) (chat, error) {
	query := `SELECT j.chat_id FROM message m JOIN chat_message_join j ON j.message_id = m.ROWID
WHERE m.is_from_me = 1 AND m.guid COLLATE BINARY = '` + strings.ReplaceAll(guid, "'", "''") + "'"
	// The bridge can return a message GUID before the joined row appears.
	deadline := time.Now().Add(8 * time.Second)
	for {
		out, err := b.read("find first message", queryMessagesDB+shellQuote(query))
		if err != nil {
			return chat{}, err
		}
		if len(bytes.TrimSpace(out)) > 0 {
			var rows []struct {
				ChatID int `json:"chat_id"`
			}
			if err := json.Unmarshal(out, &rows); err != nil {
				return chat{}, fmt.Errorf("decode first message chat: %w", err)
			}
			if len(rows) != 1 {
				return chat{}, errors.New("first message has no unique chat")
			}
			d, err := b.directory()
			if err != nil {
				return chat{}, err
			}
			c, found, err := d.match(addresses)
			if err != nil {
				return chat{}, err
			}
			if !found || c.ID != rows[0].ChatID {
				return chat{}, errors.New("first message did not resolve to the exact recipient set")
			}
			return c, nil
		}
		if !time.Now().Before(deadline) {
			return chat{}, errors.New("first message not found in Messages")
		}
		time.Sleep(250 * time.Millisecond)
	}
}
