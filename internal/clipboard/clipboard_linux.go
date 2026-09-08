//go:build linux

package clipboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"os"
	"os/exec"
	"time"
)

type toolMode int

const (
	modeNone toolMode = iota
	modeXclip
	modeWl
)

// tool wraps the external clipboard helper selected for this session.
type tool struct {
	mode    toolMode
	xclip   string // xclip binary path
	wlCopy  string // wl-copy binary path
	wlPaste string // wl-paste binary path
}

func (t *tool) name() string {
	switch t.mode {
	case modeXclip:
		return "xclip"
	case modeWl:
		return "wl-clipboard"
	}
	return "none"
}

// linuxBackend reads/writes the clipboard by delegating to xclip (X11) or
// wl-clipboard (Wayland). Both tools only expose clipboard content, not copy
// events, so change detection is poll based (see WaitForEvent).
type linuxBackend struct {
	tool     *tool
	interval time.Duration
}

// NewBackend selects xclip or wl-clipboard based on the current session.
func NewBackend(pollIntervalMs int) (Backend, error) {
	if pollIntervalMs <= 0 {
		pollIntervalMs = 1000
	}
	t, err := detectTool()
	if err != nil {
		return nil, err
	}
	return &linuxBackend{tool: t, interval: time.Duration(pollIntervalMs) * time.Millisecond}, nil
}

func detectTool() (*tool, error) {
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		wlCopy, err1 := exec.LookPath("wl-copy")
		wlPaste, err2 := exec.LookPath("wl-paste")
		if err1 == nil && err2 == nil {
			return &tool{mode: modeWl, wlCopy: wlCopy, wlPaste: wlPaste}, nil
		}
		return nil, errors.New("clipboard: Wayland session needs wl-clipboard (install wl-clipboard: apt install wl-clipboard)")
	}
	if os.Getenv("DISPLAY") != "" {
		xclip, err := exec.LookPath("xclip")
		if err == nil {
			return &tool{mode: modeXclip, xclip: xclip}, nil
		}
		return nil, errors.New("clipboard: X11 session needs xclip (install: apt install xclip)")
	}
	return nil, errors.New("clipboard: no graphical session detected (need DISPLAY for X11 or WAYLAND_DISPLAY for Wayland)")
}

// Snapshot reads the current clipboard. Text is preferred; when no text is
// available (or the text is a file:// artifact) an image is looked up.
func (b *linuxBackend) Snapshot() (Content, error) {
	text, err := b.readText()
	if err != nil {
		return Content{}, err
	}
	if text == "" {
		pngBytes, err := b.readImage()
		if err != nil {
			return Content{}, err
		}
		if len(pngBytes) > 0 && validPNG(pngBytes) {
			return Content{Kind: KindImage, PNG: pngBytes}, nil
		}
		return Content{}, nil
	}
	if IsFileArtifact(text) {
		// Copied file objects surface as text under X11 targets; file
		// transfer is unsupported so nothing is shared.
		return Content{}, nil
	}
	return Content{Kind: KindText, Text: text}, nil
}

// Write replaces the clipboard content using the external tool.
func (b *linuxBackend) Write(c Content) error {
	switch c.Kind {
	case KindEmpty:
		return nil
	case KindText:
		return b.writeText(c.Text)
	case KindImage:
		return b.writeImage(c.PNG)
	default:
		return errors.New("clipboard: unknown content kind")
	}
}

// WaitForEvent polls on a fixed interval. Content-based polling cannot detect
// re-copies of identical content, so it always reports a non-genuine wakeup.
func (b *linuxBackend) WaitForEvent(stop <-chan struct{}) bool {
	select {
	case <-stop:
		return false
	case <-time.After(b.interval):
		return false
	}
}

func (b *linuxBackend) readText() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var args []string
	switch b.tool.mode {
	case modeXclip:
		args = []string{"-selection", "clipboard", "-o"}
	case modeWl:
		args = []string{"--no-newline"}
	default:
		return "", errors.New("clipboard: no tool selected")
	}
	out, err := runOutput(ctx, b.tool.bin(), args...)
	if err != nil {
		if isExitError(err) {
			return "", nil // clipboard empty or no text target
		}
		return "", err
	}
	return string(out), nil
}

func (b *linuxBackend) readImage() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var args []string
	switch b.tool.mode {
	case modeXclip:
		args = []string{"-selection", "clipboard", "-t", "image/png", "-o"}
	case modeWl:
		args = []string{"--type", "image/png"}
	default:
		return nil, errors.New("clipboard: no tool selected")
	}
	out, err := runOutput(ctx, b.tool.bin(), args...)
	if err != nil {
		if isExitError(err) {
			return nil, nil // no image available
		}
		return nil, err
	}
	return out, nil
}

func (b *linuxBackend) writeText(text string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return runInput(ctx, b.tool.bin(), []byte(text), b.tool.writeArgs(false)...)
}

func (b *linuxBackend) writeImage(pngBytes []byte) error {
	if !validPNG(pngBytes) {
		return errors.New("clipboard: not a PNG image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return runInput(ctx, b.tool.bin(), pngBytes, b.tool.writeArgs(true)...)
}

func (t *tool) bin() string {
	switch t.mode {
	case modeXclip:
		return t.xclip
	case modeWl:
		return t.wlCopy
	}
	return ""
}

func (t *tool) writeArgs(image bool) []string {
	switch t.mode {
	case modeXclip:
		if image {
			return []string{"-selection", "clipboard", "-t", "image/png", "-i"}
		}
		return []string{"-selection", "clipboard", "-i"}
	case modeWl:
		if image {
			return []string{"--type", "image/png"}
		}
		return []string{}
	}
	return nil
}

func runOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &bytes.Buffer{}
	err := cmd.Run()
	return stdout.Bytes(), err
}

func runInput(ctx context.Context, name string, data []byte, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(data)
	cmd.Stderr = &bytes.Buffer{}
	return cmd.Run()
}

func isExitError(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee)
}

func validPNG(data []byte) bool {
	if len(data) < 8 || data[0] != 0x89 || data[1] != 'P' || data[2] != 'N' || data[3] != 'G' {
		return false
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return false
	}
	if cfg.Width*cfg.Height > 1<<26 { // guard against absurd dimensions
		return false
	}
	return true
}

// String describes the backend for log messages.
func (b *linuxBackend) String() string { return fmt.Sprintf("linux(%s)", b.tool.name()) }
