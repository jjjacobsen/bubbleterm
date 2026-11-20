package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

type attachment struct {
	Filename     string `json:"filename"`
	OriginalPath string `json:"original_path"`
	TransferName string `json:"transfer_name"`
	MIMEType     string `json:"mime_type"`
	UTI          string `json:"uti"`
	Missing      bool   `json:"missing"`
	IsSticker    bool   `json:"is_sticker"`
	TotalBytes   int64  `json:"total_bytes"`
}

func visibleAttachments(attachments []attachment) []attachment {
	var visible []attachment
	for _, a := range attachments {
		if a.isPreviewPayload() {
			continue
		}
		// imsg lists pathless preflight rows alongside the downloaded photo.
		if a.Missing && a.Filename == "" && a.OriginalPath == "" && !a.IsSticker && a.isPhoto() &&
			slices.ContainsFunc(attachments, func(photo attachment) bool {
				return !photo.Missing && photo.Filename != "" && !photo.IsSticker &&
					photo.TransferName == a.TransferName && path.Base(photo.Filename) == a.TransferName &&
					photo.MIMEType == a.MIMEType && photo.UTI == a.UTI
			}) {
			continue
		}
		visible = append(visible, a)
	}
	return visible
}

// Messages stores rich preview data as hidden plugin payload attachments.
func (a attachment) isPreviewPayload() bool {
	for _, name := range [...]string{a.TransferName, a.Filename, a.OriginalPath} {
		if strings.EqualFold(path.Ext(name), ".pluginPayloadAttachment") {
			return true
		}
	}
	return false
}

func (a attachment) isPhoto() bool {
	mime := strings.ToLower(a.MIMEType)
	if strings.HasPrefix(mime, "image/") {
		return !strings.HasPrefix(mime, "image/svg")
	}
	if mime != "" && mime != "application/octet-stream" {
		return false
	}
	for _, name := range [...]string{a.Filename, a.TransferName} {
		switch strings.ToLower(path.Ext(name)) {
		case ".jpg", ".jpeg", ".png", ".heic", ".heif", ".gif", ".webp", ".tif", ".tiff", ".bmp", ".avif":
			return true
		}
	}
	return false
}

func (a attachment) isVideo() bool {
	mime := strings.ToLower(a.MIMEType)
	if strings.HasPrefix(mime, "video/") {
		return true
	}
	if mime != "" && mime != "application/octet-stream" {
		return false
	}
	switch a.UTI {
	case "public.movie", "public.video", "public.mpeg-4", "com.apple.quicktime-movie":
		return true
	}
	for _, name := range [...]string{a.Filename, a.TransferName} {
		switch strings.ToLower(path.Ext(name)) {
		case ".mp4", ".mov", ".m4v", ".avi", ".mkv", ".webm", ".mpg", ".mpeg", ".3gp", ".3g2":
			return true
		}
	}
	return false
}

func (a attachment) key() string { return a.Filename }

func (a attachment) attachmentCommand() (string, error) {
	if a.Missing || a.Filename == "" {
		return "", errors.New("attachment is not available on the Mac")
	}
	if path.Clean(a.Filename) != a.Filename || strings.ContainsAny(a.Filename, "\x00\r\n") {
		return "", errors.New("invalid attachment path")
	}
	return "file=" + shellQuote(a.Filename) + `
root="$HOME/Library/Messages/Attachments"
case "$file" in
    '~/'*) file="$HOME/${file#\~/}" ;;
esac
case "$file" in
    "$root"/*) ;;
    *) echo 'Attachment path is outside Messages attachments' >&2; exit 1 ;;
esac
root=$(/bin/realpath "$root" 2>/dev/null) || exit 1
file=$(/bin/realpath "$file" 2>/dev/null) || { echo 'Attachment is not available on the Mac' >&2; exit 1; }
case "$file" in
    "$root"/*) ;;
    *) echo 'Resolved attachment path is outside Messages attachments' >&2; exit 1 ;;
esac
[ -f "$file" ] || { echo 'Attachment is not a regular file' >&2; exit 1; }
`, nil
}

func (a attachment) photoCommand(preview bool) (string, error) {
	if !a.isPhoto() {
		return "", errors.New("attachment is not a photo")
	}
	remote, err := a.attachmentCommand()
	if err != nil {
		return "", err
	}
	size := ""
	if preview {
		size = "256"
	}
	return remote + `tmp=$(/usr/bin/mktemp -d -t bubbleterm) || exit 1
trap '/bin/rm -rf "$tmp"' EXIT
/usr/bin/osascript -l JavaScript - "$file" "$tmp/photo.png" ` + shellQuote(size) + ` >/dev/null <<'JXA' || exit 1
ObjC.import('ImageIO')
function run([input, output, size]) {
    const source = $.CGImageSourceCreateWithURL($.NSURL.fileURLWithPath(input), null)
    const options = {
        kCGImageSourceCreateThumbnailFromImageAlways: true,
        kCGImageSourceCreateThumbnailWithTransform: true,
    }
    // Without a pixel limit, ImageIO applies the transform at full size.
    if (size) options.kCGImageSourceThumbnailMaxPixelSize = Number(size)
    const image = $.CGImageSourceCreateThumbnailAtIndex(source, $.CGImageSourceGetPrimaryImageIndex(source), $(options))
    const destination = $.CGImageDestinationCreateWithURL($.NSURL.fileURLWithPath(output), $('public.png'), 1, null)
    $.CGImageDestinationAddImage(destination, image, null)
    if (!$.CGImageDestinationFinalize(destination)) throw new Error('Cannot convert photo to PNG')
}
JXA
/bin/cat "$tmp/photo.png"`, nil
}

func (b backend) preview(a attachment) ([]byte, error) {
	file, err := b.photoFile(a, true)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(file)
}

func (b backend) openPhoto(a attachment) error {
	file, err := b.photoFile(a, false)
	if err != nil {
		return err
	}
	if err := exec.Command("xdg-open", file).Run(); err != nil {
		return fmt.Errorf("open photo viewer: %w", err)
	}
	return nil
}

func (b backend) playVideo(a attachment) error {
	player, err := exec.LookPath("mpv")
	if err != nil {
		return fmt.Errorf("play video: %w", err)
	}
	remote, err := a.attachmentCommand()
	if err != nil {
		return err
	}
	file, err := b.cachedAttachment("video-"+b.attachmentKey(a), remote+`exec /bin/cat "$file"`)
	if err != nil {
		return err
	}
	if err := exec.Command(player, "--no-terminal", "--", file).Run(); err != nil {
		return fmt.Errorf("play video: %w", err)
	}
	return nil
}

func (b backend) photoFile(a attachment, preview bool) (string, error) {
	remote, err := a.photoCommand(preview)
	if err != nil {
		return "", err
	}
	return b.cachedAttachment(fmt.Sprintf("oriented-%s-%t.png", b.attachmentKey(a), preview), remote)
}

func (b backend) attachmentKey(a attachment) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(b.host+"\x00"+a.key())))
}

func (b backend) cachedAttachment(name, remote string) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "bubbleterm")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("attachment cache is not a private directory: %s", dir)
	}

	file := filepath.Join(dir, name)
	info, err = os.Lstat(file)
	if errors.Is(err, os.ErrNotExist) {
		if err := b.cacheAttachment(dir, file, remote); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	} else if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("cached attachment is not a private regular file: %s", file)
	}
	return file, nil
}

func (b backend) cacheAttachment(dir, file, remote string) error {
	out, err := os.CreateTemp(dir, ".attachment-*")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	transferErr := b.runTo("download attachment", remote, out)
	if err := errors.Join(transferErr, out.Close()); err != nil {
		return err
	}
	return os.Rename(out.Name(), file)
}
