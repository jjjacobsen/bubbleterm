package main

import (
	"encoding/json"
	"fmt"
)

const queryMessagesDB = `exec /usr/bin/sqlite3 -batch -bail -readonly -init /dev/null -json "$HOME/Library/Messages/chat.db" `

// imsg counts older unread rows too. Use the chat's read position and the
// local unread-message index predicates. Tapbacks can count as unread.
const unreadChatsQuery = `
SELECT DISTINCT c.ROWID AS id
FROM message m
JOIN chat_message_join j ON j.message_id = m.ROWID
JOIN chat c ON c.ROWID = j.chat_id
WHERE m.is_read = 0 AND m.is_from_me = 0 AND m.item_type = 0
  AND m.is_finished = 1 AND m.is_system_message = 0 AND j.filter_action = 0
  AND m.date > c.last_read_message_timestamp`

func (b backend) unreadChats() (map[int]bool, error) {
	out, err := b.read("read unread state", queryMessagesDB+shellQuote(unreadChatsQuery))
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID int `json:"id"`
	}
	if len(out) > 0 {
		if err := json.Unmarshal(out, &rows); err != nil {
			return nil, fmt.Errorf("decode unread state: %w", err)
		}
	}
	unread := make(map[int]bool, len(rows))
	for _, row := range rows {
		unread[row.ID] = true
	}
	return unread, nil
}

const viewChatQuery = `
SELECT target.guid, CASE WHEN NOT EXISTS (SELECT 1 FROM chat other WHERE other.ROWID <> target.ROWID AND other.chat_identifier = target.chat_identifier COLLATE NOCASE) THEN target.chat_identifier END AS route, EXISTS (` + unreadChatsQuery + ` AND c.ROWID = target.ROWID) AS unread
FROM chat AS target WHERE target.ROWID = %d AND target.guid <> ''`

func (b backend) viewChat(id int) error {
	out, err := b.read("resolve Messages chat", queryMessagesDB+shellQuote(fmt.Sprintf(viewChatQuery, id)))
	if err != nil {
		return err
	}
	if len(out) == 0 {
		return fmt.Errorf("chat %d has no Messages GUID", id)
	}
	var rows []struct {
		GUID   string `json:"guid"`
		Route  string `json:"route"`
		Unread int    `json:"unread"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return fmt.Errorf("decode Messages chat: %w", err)
	}
	target := rows[0]
	if target.Unread == 0 {
		return nil
	}

	check := "SELECT EXISTS (" + unreadChatsQuery + fmt.Sprintf(" AND c.ROWID = %d)", id)
	command := `/usr/bin/osascript -l JavaScript - ` + shellQuote(target.GUID) + " " + shellQuote(target.Route) + ` <<'JXA' || failed=1
` + markChatReadAX + `
JXA
for n in 0 1 2 3 4 5; do
  [ "$n" = 0 ] || /bin/sleep 1
  state=$(/usr/bin/sqlite3 -batch -bail -readonly -init /dev/null "$HOME/Library/Messages/chat.db" ` + shellQuote(check) + `) || exit 1
  [ "$state" = 0 ] && exit 0
  [ "${failed:-0}" = 0 ] || exit 1
done
echo 'Messages still reports this chat as unread after Mark as Read.' >&2
exit 1`
	return b.run("mark chat read", "/bin/sh -c "+shellQuote(command))
}

const markChatReadAX = `
ObjC.import('Cocoa');
ObjC.bindFunction('CGSessionCopyCurrentDictionary', ['id', []]);
ObjC.bindFunction('AXIsProcessTrusted', ['bool', []]);
ObjC.bindFunction('AXUIElementCreateApplication', ['id', ['int']]);
ObjC.bindFunction('AXUIElementSetMessagingTimeout', ['int', ['id', 'float']]);
ObjC.bindFunction('AXUIElementCopyAttributeValue', ['int', ['id', 'id', 'id*']]);
ObjC.bindFunction('AXUIElementCopyActionNames', ['int', ['id', 'id*']]);
ObjC.bindFunction('AXUIElementPerformAction', ['int', ['id', 'id']]);
ObjC.bindFunction('CFEqual', ['bool', ['id', 'id']]);

function requireConsole() {
    const session = ObjC.deepUnwrap($.CGSessionCopyCurrentDictionary());
    if (session && session.CGSSessionScreenIsLocked) throw new Error('Unlock the Mac first');
    if (!session || !session.kCGSSessionOnConsoleKey || !session.kCGSessionLoginDoneKey ||
        session.kCGSSessionUserNameKey !== ObjC.unwrap($.NSUserName())) {
        throw new Error('Log in to the Mac desktop as the SSH user first');
    }
}

function value(el, key) {
    const result = Ref();
    const status = $.AXUIElementCopyAttributeValue(el, key, result);
    if (status === 0) return ObjC.unwrap(result[0]);
    if (status === -25205 || status === -25212) return null;
    throw new Error('Cannot read Messages ' + key + ' (AX error ' + status + ')');
}

const children = el => value(el, 'AXChildren') || [];
const identifier = el => value(el, 'AXIdentifier');

// Follow only anonymous layout groups. Do not descend into the transcript,
// composer, search controls, or conversation rows.
function sidebars(root, depth) {
    if (!depth) return [];
    return children(root).flatMap(el => {
        const id = identifier(el);
        if (id === 'ConversationList') return [el];
        return !id && value(el, 'AXRole') === 'AXGroup' ? sidebars(el, depth - 1) : [];
    });
}

function targetRow(sidebar, guid) {
    const matches = id => id === 'pinned-' + guid || id === 'list-' + guid;
    const rows = children(sidebar);
    const exact = rows.filter(row => matches(identifier(row)));
    if (exact.length > 1) throw new Error('More than one Messages sidebar row matches this chat');
    if (exact.length === 1) return exact[0];

    // Some versions expose the GUID only in Messages' selection preference.
    // Use it only with a single live, selected sidebar row that has no AX ID.
    const shell = Application.currentApplication();
    shell.includeStandardAdditions = true;
    const selected = value(sidebar, 'AXSelectedChildren') || [];
    if (selected.length === 1 && !identifier(selected[0]) && value(selected[0], 'AXSelected') &&
        rows.some(row => $.CFEqual(row, selected[0])) && matches(shell.doShellScript(
            '/usr/bin/defaults read com.apple.MobileSMS CKLastSelectedItemIdentifier 2>/dev/null || true'))) {
        return selected[0];
    }
    throw new Error('Cannot identify this chat in the Messages sidebar. Select it on the Mac and retry');
}

function run(args) {
    requireConsole();
    if (!$.AXIsProcessTrusted()) throw new Error('Grant Accessibility access on the Mac to the SSH process');
    const selected = $.NSUserDefaults.alloc.initWithSuiteName('com.apple.MobileSMS').stringForKey('CKLastSelectedItemIdentifier');
    const current = selected && ObjC.unwrap(selected);
    if (args[1] && args[1] !== '--dry-run' && !['list-' + args[0], 'pinned-' + args[0]].includes(current)) {
        const url = $.NSURL.URLWithString('sms://open?groupid=' + encodeURIComponent(args[1]));
        if (!$.NSWorkspace.sharedWorkspace.openURL(url)) throw new Error('Cannot open the selected Messages chat');
        delay(0.5);
    }
    const apps = $.NSRunningApplication.runningApplicationsWithBundleIdentifier('com.apple.MobileSMS');
    if (Number(apps.count) !== 1) throw new Error('Open Messages on the Mac first');
    const running = apps.firstObject;
    if (!running.isActive && args[1] !== '--dry-run') {
        running.activateWithOptions($.NSApplicationActivateIgnoringOtherApps);
        delay(0.25);
    }
    const app = $.AXUIElementCreateApplication(running.processIdentifier);
    const timeout = $.AXUIElementSetMessagingTimeout(app, 1);
    if (timeout !== 0) throw new Error('Cannot set the Messages AX timeout (AX error ' + timeout + ')');
    const focusedWindow = () => {
        if (!running.isActive) throw new Error('Bring Messages to the front on the Mac first');
        const windows = (value(app, 'AXWindows') || []).filter(el => value(el, 'AXSubrole') === 'AXStandardWindow');
        const focused = value(app, 'AXFocusedWindow');
        if (windows.length !== 1 || !focused || !$.CFEqual(windows[0], focused) || value(focused, 'AXMinimized')) {
            throw new Error('Keep one Messages window open with its sidebar visible');
        }
        return focused;
    };
    const window = focusedWindow();
    const found = sidebars(window, 6);
    if (found.length !== 1) throw new Error('Cannot find one Messages conversation sidebar');
    const row = targetRow(found[0], args[0]);
    const result = Ref();
    const status = $.AXUIElementCopyActionNames(row, result);
    if (status !== 0) throw new Error('Cannot read Messages sidebar actions (AX error ' + status + ')');
    const actions = ObjC.deepUnwrap(result[0]).filter(name => /^(Name:)?Mark as Read(?:\n|$)/.test(name));
    if (actions.length !== 1 || !value(row, 'AXEnabled')) {
        throw new Error('This Messages sidebar row does not offer Mark as Read');
    }
    if (args[1] === '--dry-run') return 'Exact sidebar target offers Mark as Read. No action performed.';
    if (!$.CFEqual(window, focusedWindow()) || !$.CFEqual(row, targetRow(found[0], args[0]))) {
        throw new Error('Messages switched chats or windows');
    }
    requireConsole();
    const performed = $.AXUIElementPerformAction(row, actions[0]);
    // If Messages times out, verify the read position without retrying the action.
    if (performed !== 0 && performed !== -25204) throw new Error('Cannot mark this chat as read (AX error ' + performed + ')');
}
`
