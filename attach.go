package main

import (
	"errors"
	"fmt"
	"mime"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
)

type draftFile struct {
	Path, Name string
	Size       int64
	Temporary  bool
}

func (file *draftFile) cleanup() {
	if file != nil && file.Temporary {
		os.Remove(file.Path)
	}
}

func pasteClipboard() (text string, file *draftFile, err error) {
	out, err := exec.Command("wl-paste", "--list-types").Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && strings.TrimSpace(string(exit.Stderr)) == "Nothing is copied" {
			return "", nil, nil
		}
		return "", nil, fmt.Errorf("list clipboard types: %w", err)
	}
	types := strings.Split(string(out), "\n")
	var imageErr error
	for _, kind := range types {
		if !draftPhotoType(kind) {
			continue
		}
		file, imageErr = pasteClipboardImage(kind)
		if imageErr == nil {
			return "", file, nil
		}
	}
	for _, preferred := range []string{"text/plain;charset=utf-8", "UTF8_STRING", "text/plain", "TEXT", "STRING"} {
		for _, kind := range types {
			if strings.EqualFold(kind, preferred) {
				out, err := exec.Command("wl-paste", "--no-newline", "--type", kind).Output()
				if err != nil {
					return "", nil, fmt.Errorf("paste clipboard text: %w", err)
				}
				return string(out), nil, nil
			}
		}
	}
	return "", nil, imageErr
}

func pasteClipboardImage(kind string) (_ *draftFile, err error) {
	out, err := os.CreateTemp("", "bubbleterm-screenshot-*")
	if err != nil {
		return nil, err
	}
	file := &draftFile{Path: out.Name(), Temporary: true}
	defer func() {
		if err != nil {
			file.cleanup()
		}
	}()

	cmd := exec.Command("wl-paste", "--no-newline", "--type", kind)
	cmd.Stdout = out
	if err := errors.Join(cmd.Run(), out.Close()); err != nil {
		return nil, fmt.Errorf("paste clipboard image: %w", err)
	}
	size, kind, err := draftMedia(file.Path)
	if err != nil {
		return nil, err
	}
	if !draftPhotoType(kind) {
		return nil, errors.New("clipboard image is not a photo")
	}
	var ext string
	switch kind {
	case "image/jpeg":
		ext = ".jpg"
	case "image/heic", "image/heic-sequence":
		ext = ".heic"
	default:
		extensions, err := mime.ExtensionsByType(kind)
		if err != nil {
			return nil, err
		}
		if len(extensions) == 0 {
			return nil, fmt.Errorf("unsupported clipboard image format: %s", kind)
		}
		ext = extensions[0]
	}
	file.Name = "Screenshot" + ext
	file.Size = size
	return file, nil
}

func pickFile() (*draftFile, error) {
	out, err := exec.Command("zenity", "--file-selection", "--title=Attach photo or video").Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("pick attachment: %w", err)
	}
	path := strings.TrimSuffix(string(out), "\n")
	size, kind, err := draftMedia(path)
	if err != nil {
		return nil, err
	}
	if !draftPhotoType(kind) && !strings.HasPrefix(kind, "video/") {
		return nil, fmt.Errorf("not a photo or video: %q", filepath.Base(path))
	}
	name := strings.Map(func(r rune) rune {
		if r == '\\' || r == ':' || unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			return '_'
		}
		return r
	}, filepath.Base(path))
	return &draftFile{Path: path, Name: name, Size: size}, nil
}

func draftMedia(path string) (size int64, kind string, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, "", err
	}
	if !info.Mode().IsRegular() {
		return 0, "", fmt.Errorf("not a regular file: %q", path)
	}
	out, err := exec.Command("file", "--brief", "--dereference", "--mime-type", "--", path).Output()
	if err != nil {
		return 0, "", fmt.Errorf("identify attachment: %w", err)
	}
	return info.Size(), strings.TrimSpace(string(out)), nil
}

func draftPhotoType(kind string) bool {
	return strings.HasPrefix(kind, "image/") && !strings.HasPrefix(kind, "image/svg")
}
