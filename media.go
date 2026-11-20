package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
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

func (a attachment) key() string { return a.Filename }

func (a attachment) photoCommand(preview bool) (string, error) {
	if !a.isPhoto() {
		return "", errors.New("attachment is not a photo")
	}
	if a.Missing || a.Filename == "" {
		return "", errors.New("photo is not available on the Mac")
	}
	if path.Clean(a.Filename) != a.Filename || strings.ContainsAny(a.Filename, "\x00\r\n") {
		return "", errors.New("invalid photo path")
	}
	size := ""
	if preview {
		size = "256"
	}
	return "photo=" + shellQuote(a.Filename) + `
root="$HOME/Library/Messages/Attachments"
case "$photo" in
    '~/'*) photo="$HOME/${photo#\~/}" ;;
esac
case "$photo" in
    "$root"/*) ;;
    *) echo 'Photo path is outside Messages attachments' >&2; exit 1 ;;
esac
root=$(/bin/realpath "$root" 2>/dev/null) || exit 1
photo=$(/bin/realpath "$photo" 2>/dev/null) || { echo 'Photo is not available on the Mac' >&2; exit 1; }
case "$photo" in
    "$root"/*) ;;
    *) echo 'Resolved photo path is outside Messages attachments' >&2; exit 1 ;;
esac
[ -f "$photo" ] || { echo 'Photo is not a regular file' >&2; exit 1; }
tmp=$(/usr/bin/mktemp -d -t bubbleterm) || exit 1
trap '/bin/rm -rf "$tmp"' EXIT
/usr/bin/osascript -l JavaScript - "$photo" "$tmp/photo.png" ` + shellQuote(size) + ` >/dev/null <<'JXA' || exit 1
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

func (b backend) photoFile(a attachment, preview bool) (string, error) {
	remote, err := a.photoCommand(preview)
	if err != nil {
		return "", err
	}
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
		return "", fmt.Errorf("photo cache is not a private directory: %s", dir)
	}

	key := sha256.Sum256([]byte(b.host + "\x00" + a.key()))
	file := filepath.Join(dir, fmt.Sprintf("oriented-%x-%t.png", key, preview))
	info, err = os.Lstat(file)
	if errors.Is(err, os.ErrNotExist) {
		if err := b.cachePhoto(dir, file, remote); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	} else if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("cached photo is not a private regular file: %s", file)
	}
	return file, nil
}

func (b backend) cachePhoto(dir, file, remote string) error {
	out, err := os.CreateTemp(dir, ".photo-*.png")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	transferErr := b.runTo("download photo", remote, out)
	if err := errors.Join(transferErr, out.Close()); err != nil {
		return err
	}
	return os.Rename(out.Name(), file)
}
