# BubbleTerm

A keyboard-driven Messages client for Linux. BubbleTerm runs in your terminal and connects over SSH to a Mac signed in to Messages. The Mac sends and receives messages. No public server or disabled SIP required

## Setup

1. On the Mac, enable Remote Login and set up [imsg](https://github.com/openclaw/imsg) (`brew install steipete/tap/imsg`). Grant the SSH session Full Disk Access to read history. Sending and UI actions can also require Automation and Accessibility permissions. UI actions need an unlocked Messages session. Rich link previews also need `uv` and `/usr/bin/python3` on the Mac
2. From Linux, configure SSH key access to the Mac. The default SSH host is `mac`. Verify access with `ssh -o BatchMode=yes mac true`. Tailscale works for access away from home
3. From this repo, install the Mac helpers for reactions, replies, edits, and new groups:

   ```sh
   ssh mac 'mkdir -p ~/.local/bin'
   tar -C mac -cf - . | ssh mac 'tar -C ~/.local/bin -xf -'
   ```

4. On Omarchy, install and launch BubbleTerm:

   ```sh
   mise install
   mise run install
   bubbleterm
   ```

You can also launch **BubbleTerm** from the app launcher. For another SSH host, run `bubbleterm -host HOST`. For development, run `mise run chat`

## Controls

Press `?` outside text input to see all controls. Press `Esc` or `?` to close help

| Key | Action |
|---|---|
| `j` / `k`, `Enter`, `Esc` | Navigate, open, go back |
| `i`, then `Enter` | Compose and send. Shift+Enter adds a line |
| `Ctrl+V` / `Ctrl+O` | Paste a screenshot or pick a photo or video while composing |
| `r` / `R` / `e` | React, reply in a thread, or edit your iMessage |
| `/` | Search loaded chats or messages. Ctrl+J and Ctrl+K cycle matches |
| `n` / `p` / `h` | Start, pin, or hide a chat from the sidebar. Shift+H shows hidden chats |
| `o` / `q` | Load older messages / quit outside the composer |

Ghostty supports inline photos. Enter opens a selected photo, video, or link. Pins and hidden chats are local to BubbleTerm

[MIT License](LICENSE)
