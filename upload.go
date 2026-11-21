package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"syscall"
)

var (
	errUploadFailed  = errors.New("attachment upload failed")
	errSendUncertain = errors.New("attachment send outcome unknown. Check Messages before retrying")
)

func (b backend) sendFile(id int, text string, a draftFile, progress func(int64)) error {
	if strings.ContainsRune(text, '\x00') {
		return fmt.Errorf("%w: message text contains a NUL byte", errUploadFailed)
	}
	s, tmp, err := b.uploadFile(a, progress)
	if err != nil {
		return fmt.Errorf("%w: %w", errUploadFailed, err)
	}

	// imsg copies the file into its own Messages staging directory before dispatch.
	// Remove only our source, and only after imsg exits. Leave its copy for Messages.
	remote := "tmp=" + shellQuote(tmp) + `
trap '/bin/rm -rf "$tmp"' EXIT
` + imsgPath + fmt.Sprintf(`imsg send --chat-id %d --file "$tmp"/`, id) + shellQuote(a.Name)
	if text != "" {
		remote += " " + shellQuote("--text="+text)
	}
	cmd := s.ssh(s.ctx, "--", b.host, "/bin/sh -c "+shellQuote(remote))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// The SSH channel can fail after Messages accepts the send. Do not repeat it.
		return fmt.Errorf("%w: %w", errSendUncertain, remoteError("imsg send", err, stderr.Bytes()))
	}
	return nil
}

func (b backend) uploadFile(a draftFile, progress func(int64)) (*sshSession, string, error) {
	if a.Name == "" || a.Name == "." || a.Name == ".." || strings.ContainsAny(a.Name, "/\x00\r\n") {
		return nil, "", errors.New("invalid attachment name")
	}
	// O_NONBLOCK lets Stat reject a FIFO without waiting for a writer.
	f, err := os.OpenFile(a.Path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, "", err
	}
	if !info.Mode().IsRegular() || info.Size() != a.Size {
		return nil, "", errors.New("attachment is not a regular file of the selected size")
	}

	s, err := b.open()
	if err != nil {
		return nil, "", err
	}
	remote := `umask 077
# imsg rejects source paths with symlink components, such as /var on macOS.
cd -P "${TMPDIR:-/tmp}" || exit 1
tmp=$(/usr/bin/mktemp -d "$PWD/bubbleterm-upload.XXXXXXXX") || exit 1
trap '/bin/rm -rf "$tmp"' EXIT
trap 'exit 1' HUP INT TERM
file="$tmp"/` + shellQuote(a.Name) + `
/bin/cat > "$file" || exit 1
size=$(/usr/bin/wc -c < "$file") || exit 1
[ "$size" -eq ` + fmt.Sprint(a.Size) + ` ] || { echo 'Incomplete or changed attachment' >&2; exit 1; }
printf '%s\n' "$tmp" || exit 1
trap - EXIT`
	cmd := s.ssh(s.ctx, "--", b.host, "/bin/sh -c "+shellQuote(remote))
	var stdout, stderr bytes.Buffer
	cmd.Stdin = &uploadReader{Reader: io.LimitReader(f, a.Size+1), progress: progress}
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, "", remoteError("stream attachment", err, stderr.Bytes())
	}
	tmp := strings.TrimSuffix(stdout.String(), "\n")
	if !path.IsAbs(tmp) || path.Clean(tmp) != tmp || !strings.HasPrefix(path.Base(tmp), "bubbleterm-upload.") || strings.ContainsAny(tmp, "\x00\r\n") {
		return nil, "", errors.New("invalid remote upload path")
	}
	return s, tmp, nil
}

type uploadReader struct {
	io.Reader
	progress func(int64)
	read     int64
}

func (r *uploadReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 && r.progress != nil {
		r.read += int64(n)
		r.progress(r.read)
	}
	return n, err
}
