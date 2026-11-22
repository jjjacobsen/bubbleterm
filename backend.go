package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

type backend struct{ host string }

type chat struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	IsGroup     bool   `json:"is_group"`
	Unread      bool   `json:"unread"`
	ContactName string `json:"contact_name,omitempty"`
	Identifier  string `json:"identifier,omitempty"`
}

type message struct {
	ID                 int          `json:"id"`
	GUID               string       `json:"guid"`
	Text               string       `json:"text"`
	Sender             string       `json:"sender"`
	SenderName         string       `json:"sender_name"`
	FromMe             bool         `json:"is_from_me"`
	Editable           bool         `json:"-"`
	CreatedAt          time.Time    `json:"created_at"`
	UnsentAt           *time.Time   `json:"unsent_at"`
	BalloonBundleID    string       `json:"balloon_bundle_id"`
	Attachments        []attachment `json:"attachments"`
	Links              []string     `json:"-"`
	Thread             threadOrigin `json:"-"`
	MyReaction         string       `json:"-"`
	OtherReactions     string       `json:"-"`
	SelectedAttachment *attachment  `json:"-"`
}

const imsgPath = "PATH=/opt/homebrew/bin:/usr/local/bin:$PATH "
const imsg = imsgPath + "exec imsg "
const readTimeout = 15 * time.Second

// imsg can outlive a non-interactive SSH channel. Stop it when SSH stdin closes.
const watchCommand = `exec 3<&0
` + imsgPath + `imsg watch --reactions --json &
watcher=$!
( read -r ignored <&3; kill "$watcher" 2>/dev/null ) &
monitor=$!
wait "$watcher"
status=$?
kill "$monitor" 2>/dev/null
exit "$status"`

// Backend values for the same host share one session, even when the UI copies them.
var backendSessions = struct {
	sync.Mutex
	byHost map[string]*sshSession
}{byHost: make(map[string]*sshSession)}

type sshSession struct {
	ctx    context.Context
	cancel context.CancelFunc
	dir    string

	mu      sync.Mutex
	started bool
}

func (b backend) session() (*sshSession, error) {
	backendSessions.Lock()
	defer backendSessions.Unlock()
	if s, ok := backendSessions.byHost[b.host]; ok {
		if s == nil {
			return nil, context.Canceled
		}
		return s, nil
	}
	// A short, private path keeps the control socket within the Unix socket limit.
	dir, err := os.MkdirTemp("/tmp", "bubbleterm-")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &sshSession{ctx: ctx, cancel: cancel, dir: dir}
	backendSessions.byHost[b.host] = s
	return s, nil
}

func (s *sshSession) ssh(ctx context.Context, opts ...string) *exec.Cmd {
	args := []string{
		"-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=2",
		"-o", "ControlMaster=auto", "-o", "ControlPersist=60", "-S", s.dir + "/socket",
	}
	return exec.CommandContext(ctx, "ssh", append(args, opts...)...)
}

func (s *sshSession) start(host string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil
	}
	// Establish the master before parallel reads and watch can race to create it.
	ctx, cancel := context.WithTimeout(s.ctx, readTimeout)
	defer cancel()
	out, err := s.ssh(ctx, "-fN", "--", host).CombinedOutput()
	if err != nil {
		return remoteError("open SSH connection", errors.Join(ctx.Err(), err), out)
	}
	s.started = true
	return nil
}

func (b backend) close() error {
	backendSessions.Lock()
	s := backendSessions.byHost[b.host]
	backendSessions.byHost[b.host] = nil
	backendSessions.Unlock()
	if s == nil {
		return nil
	}
	s.cancel()
	s.mu.Lock()
	defer s.mu.Unlock()

	var exitErr error
	if _, err := os.Stat(s.dir + "/socket"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := s.ssh(ctx, "-O", "exit", "--", b.host).CombinedOutput()
		if err != nil {
			exitErr = remoteError("close SSH connection", err, out)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		exitErr = err
	}
	return errors.Join(exitErr, os.RemoveAll(s.dir))
}

func (b backend) open() (*sshSession, error) {
	s, err := b.session()
	if err != nil {
		return nil, err
	}
	if err := s.start(b.host); err != nil {
		return nil, err
	}
	return s, nil
}

func remoteError(op string, err error, out []byte) error {
	if detail := strings.TrimSpace(string(out)); detail != "" {
		return fmt.Errorf("%s: %w: %s", op, err, detail)
	}
	return fmt.Errorf("%s: %w", op, err)
}

func (b backend) read(op, remote string) ([]byte, error) {
	var out bytes.Buffer
	if err := b.runTo(op, remote, &out, readTimeout); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (b backend) run(op, remote string) error {
	return b.runTo(op, remote, io.Discard, 0)
}

func (b backend) runTo(op, remote string, out io.Writer, timeout time.Duration) error {
	s, err := b.open()
	if err != nil {
		return err
	}
	ctx := s.ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := s.ssh(ctx, "--", b.host, remote)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = out, &stderr
	if err := cmd.Run(); err != nil {
		return remoteError(op, errors.Join(ctx.Err(), err), stderr.Bytes())
	}
	return nil
}

func readRows[T any](b backend, op, remote string) ([]T, error) {
	out, err := b.read(op, imsg+remote)
	if err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(out))
	var rows []T
	for {
		var row T
		if err := d.Decode(&row); err == io.EOF {
			return rows, nil
		} else if err != nil {
			return nil, fmt.Errorf("decode %s: %w", op, err)
		}
		rows = append(rows, row)
	}
}

func (b backend) chats() ([]chat, map[int]bool, error) {
	chats, err := readRows[chat](b, "list chats", "chats --limit 100 --json")
	if err != nil {
		return nil, nil, err
	}
	unread, err := b.unreadChats()
	if err != nil {
		return nil, nil, err
	}
	for i := range chats {
		chats[i].Unread = unread[chats[i].ID]
	}
	return chats, unread, nil
}

func (b backend) history(id, limit int) ([]message, bool, error) {
	// imsg has no row cursor. The extra row tells us whether another page exists.
	rows, err := readRows[message](b, "read history", fmt.Sprintf("history --chat-id %d --limit %d --attachments --json", id, limit+1))
	if err != nil {
		return nil, false, err
	}
	more := len(rows) > limit
	rows = rows[:min(len(rows), limit)]
	for i := range rows {
		rows[i].Attachments = visibleAttachments(rows[i].Attachments)
		if rows[i].SenderName == "" {
			rows[i].SenderName = rows[i].Sender
		}
		if rows[i].UnsentAt != nil && rows[i].Text == "" && len(rows[i].Attachments) == 0 {
			rows[i].Text = "[Unsent message]"
		}
	}
	if err := b.loadEditEligibility(rows); err != nil {
		return nil, false, err
	}
	if err := b.loadHistoryDetails(rows); err != nil {
		return nil, false, err
	}
	if err := b.loadHistoryLinks(rows); err != nil {
		return nil, false, err
	}
	if err := b.loadHistoryThreads(rows); err != nil {
		return nil, false, err
	}
	slices.Reverse(rows)
	return rows, more, nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func (b backend) send(id int, text string) error {
	return b.run("send message", imsg+fmt.Sprintf("send --chat-id %d %s", id, shellQuote("--text="+text)))
}

func (b backend) react(id int, m message, kind string) error {
	command := fmt.Sprintf(`exec "$HOME/.local/bin/bubbleterm-react" %d %s %s`, id, shellQuote(m.GUID), shellQuote(kind))
	if m.SelectedAttachment != nil {
		command += " --attachment " + shellQuote(m.SelectedAttachment.Filename)
	}
	return b.run("react to message", command)
}

// watch blocks until the stream stops or close cancels it.
func (b backend) watch(refresh func(), onEvent func(context.Context, json.RawMessage)) error {
	s, err := b.open()
	if err != nil {
		return err
	}
	cmd := s.ssh(s.ctx, "--", b.host, watchCommand)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start watch: %w", err)
	}
	d := json.NewDecoder(stdout)
	var row json.RawMessage
	for {
		if err := d.Decode(&row); err != nil {
			if err != io.EOF {
				_ = cmd.Process.Kill()
			}
			waitErr := cmd.Wait()
			if s.ctx.Err() != nil {
				return s.ctx.Err()
			}
			if err != io.EOF {
				return fmt.Errorf("decode watch event: %w", err)
			}
			if waitErr != nil {
				return remoteError("watch", waitErr, stderr.Bytes())
			}
			return io.EOF
		}
		refresh()
		if onEvent != nil {
			onEvent(s.ctx, row)
		}
	}
}
