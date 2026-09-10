//go:build linux

package clipboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/image/bmp"
	"golang.org/x/image/tiff"
	"golang.org/x/image/webp"
)

type toolMode int

const (
	modeNone toolMode = iota
	modeXclip
	modeWl
)

// tool wraps the external clipboard helper selected for this session.
type tool struct {
	mode      toolMode
	xclip     string // xclip binary path
	wlCopy    string // wl-copy binary path
	wlPaste   string // wl-paste binary path
	listTypes bool   // the tool can enumerate the formats the owner offers
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

// bin returns the binary used to *write* the clipboard (wl-copy / xclip -i).
func (t *tool) bin() string {
	switch t.mode {
	case modeXclip:
		return t.xclip
	case modeWl:
		return t.wlCopy
	}
	return ""
}

// readBin returns the binary used to *read* the clipboard (xclip / wl-paste).
// wl-copy must never be used for reads: it copies its arguments onto the
// clipboard instead of pasting.
func (t *tool) readBin() string {
	switch t.mode {
	case modeXclip:
		return t.xclip
	case modeWl:
		return t.wlPaste
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

// linuxBackend reads/writes the clipboard by delegating to xclip (X11) or
// wl-clipboard (Wayland). On X11 the backend additionally subscribes to
// XFixes selection-owner notifications, so WaitForEvent wakes up at the moment
// of every genuine copy instead of on a fixed poll cadence. Wayland has no
// such event channel, so it is always poll based (see WaitForEvent).
type linuxBackend struct {
	tool     *tool
	interval time.Duration

	events      *xfixesEvents // X11 XFixes event source; nil while unavailable
	eventsTried bool          // only attempt to open the X connection once
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
			// Enumerating offered MIME types needs --list-types, which exists
			// since wl-clipboard 2.0. Older releases fall back to guessing.
			return &tool{
				mode:      modeWl,
				wlCopy:    wlCopy,
				wlPaste:   wlPaste,
				listTypes: wlPasteSupportsListTypes(wlPaste),
			}, nil
		}
		return nil, errors.New("clipboard: Wayland session needs wl-clipboard (install wl-clipboard: apt install wl-clipboard)")
	}
	if os.Getenv("DISPLAY") != "" {
		xclip, err := exec.LookPath("xclip")
		if err == nil {
			return &tool{mode: modeXclip, xclip: xclip, listTypes: true}, nil
		}
		return nil, errors.New("clipboard: X11 session needs xclip (install: apt install xclip)")
	}
	return nil, errors.New("clipboard: no graphical session detected (need DISPLAY for X11 or WAYLAND_DISPLAY for Wayland)")
}

// wlPasteSupportsListTypes reports whether `wl-paste --list-types` exists
// (wl-clipboard 2.x) by parsing the version banner, which looks like
// "wl-clipboard 2.2.1".
func wlPasteSupportsListTypes(wlPaste string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := runOutput(ctx, wlPaste, "--version")
	if err != nil {
		return false
	}
	return firstInteger(string(out)) >= 2
}

// firstInteger scans s for the first decimal integer and returns it (-1 when
// there is none).
func firstInteger(s string) int {
	n := -1
	started := false
	for _, r := range s {
		if r >= '0' && r <= '9' {
			if !started {
				started = true
				n = 0
			}
			n = n*10 + int(r-'0')
			if n > 10000 {
				return n
			}
		} else if started {
			break
		}
	}
	return n
}

// Snapshot reads the current clipboard. Whenever the owner advertises the
// formats it offers, the advertised types drive the read: a copied file list
// is ignored, an image is preferred over text (mirroring the Windows CF_DIB
// preference) and non-PNG image encodings are converted to PNG on the fly.
// Only tools that cannot enumerate types (wl-clipboard < 2) fall back to the
// historical text-then-PNG guess in snapshotLegacy.
func (b *linuxBackend) Snapshot() (Content, error) {
	if !b.tool.listTypes {
		return b.snapshotLegacy()
	}
	offered, err := b.listTargets()
	if err != nil {
		if isExitError(err) {
			return Content{}, nil // clipboard empty or owner gone
		}
		return Content{}, err
	}
	if len(offered) == 0 {
		return Content{}, nil
	}
	return snapshotFromTargets(offered, b.fetchTarget)
}

// snapshotLegacy is the old heuristic, kept for wl-clipboard versions without
// --list-types: read plain text and, when the clipboard holds no text, try an
// image/png fetch.
func (b *linuxBackend) snapshotLegacy() (Content, error) {
	text, err := b.readTextLegacy()
	if err != nil {
		return Content{}, err
	}
	if text == "" {
		pngBytes, err := b.readImagePNG()
		if err != nil {
			return Content{}, err
		}
		if len(pngBytes) > 0 && validPNG(pngBytes) {
			return Content{Kind: KindImage, PNG: pngBytes}, nil
		}
		return Content{}, nil
	}
	if IsFileArtifact(text) {
		return Content{}, nil
	}
	return Content{Kind: KindText, Text: text}, nil
}

// listTargets returns the target names (X11 atom names / Wayland MIME types)
// the current clipboard owner offers, one per line. An empty result means the
// clipboard is empty.
func (b *linuxBackend) listTargets() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var args []string
	switch b.tool.mode {
	case modeXclip:
		args = []string{"-selection", "clipboard", "-t", "TARGETS", "-o"}
	case modeWl:
		args = []string{"--list-types"}
	default:
		return nil, errors.New("clipboard: no tool selected")
	}
	out, err := runOutput(ctx, b.tool.readBin(), args...)
	if err != nil {
		if isExitError(err) {
			return nil, nil // clipboard empty or owner gone
		}
		return nil, err
	}
	return parseTargets(string(out)), nil
}

// fetchTarget reads the clipboard bytes advertised under one specific target
// name. errNoTarget means the owner no longer offers that target (for
// instance the selection changed between listing and reading).
func (b *linuxBackend) fetchTarget(mime string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var args []string
	switch b.tool.mode {
	case modeXclip:
		args = []string{"-selection", "clipboard", "-t", mime, "-o"}
	case modeWl:
		args = []string{"--no-newline", "--type", mime}
	default:
		return nil, errors.New("clipboard: no tool selected")
	}
	out, err := runOutput(ctx, b.tool.readBin(), args...)
	if err != nil {
		if isExitError(err) {
			return nil, errNoTarget
		}
		return nil, err
	}
	return out, nil
}

func (b *linuxBackend) readTextLegacy() (string, error) {
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
	out, err := runOutput(ctx, b.tool.readBin(), args...)
	if err != nil {
		if isExitError(err) {
			return "", nil // clipboard empty or no text target
		}
		return "", err
	}
	return string(out), nil
}

func (b *linuxBackend) readImagePNG() ([]byte, error) {
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
	out, err := runOutput(ctx, b.tool.readBin(), args...)
	if err != nil {
		if isExitError(err) {
			return nil, nil // no image available
		}
		return nil, err
	}
	return out, nil
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

// WaitForEvent blocks until a clipboard change is likely or stop is closed.
// On X11 an XFixes subscription reports every selection-owner change at the
// moment it happens (a genuine event). Everywhere else — and as a safety net
// on X11 when the X connection or the XFixes extension is unavailable — it
// wakes on the poll interval instead.
func (b *linuxBackend) WaitForEvent(stop <-chan struct{}) bool {
	if b.tool.mode == modeXclip && b.events == nil && !b.eventsTried {
		b.eventsTried = true
		ev, err := newXfixesEvents()
		if err == nil {
			b.events = ev
		}
	}
	if b.events != nil {
		select {
		case <-b.events.changed:
			return true
		case <-stop:
			b.events.close()
			return false
		case <-time.After(10 * b.interval):
			// Heartbeat poll: catches changes the event stream could not
			// deliver (e.g. the X server dropped the connection).
			return false
		}
	}
	select {
	case <-stop:
		return false
	case <-time.After(b.interval):
		return false
	}
}

// String describes the backend for log messages.
func (b *linuxBackend) String() string {
	return fmt.Sprintf("linux(%s)", b.tool.name())
}

// --- format-aware snapshot helpers ------------------------------------------

// errNoTarget marks "the clipboard owner offers no such target right now",
// which is not a failure worth reporting: the selection simply changed (or
// was cleared) between the type listing and the read.
var errNoTarget = errors.New("clipboard: requested target not available")

// parseTargets splits the output of a target listing (xclip TARGETS /
// wl-paste --list-types) into individual target names.
func parseTargets(out string) []string {
	return strings.Fields(out)
}

func offerSet(offered []string) map[string]bool {
	set := make(map[string]bool, len(offered))
	for _, t := range offered {
		if t = strings.TrimSpace(t); t != "" {
			set[t] = true
		}
	}
	return set
}

func hasAny(set map[string]bool, names ...string) bool {
	for _, n := range names {
		if set[n] {
			return true
		}
	}
	return false
}

// fileTargets are the formats file managers offer when file *objects* are
// copied. File transfer is out of scope, so their presence suppresses sharing
// even when an image preview or text rendition rides along.
var fileTargets = []string{
	"text/uri-list",
	"x-special/gnome-copied-files",
	"application/x-kde4-urilist",
}

// imageFormats lists the image encodings share-clip understands, in
// preference order. For each entry, names holds the MIME spellings
// applications actually put on the clipboard.
var imageFormats = []struct {
	canonical string
	names     []string
}{
	{"image/png", []string{"image/png", "image/x-png"}},
	{"image/jpeg", []string{"image/jpeg", "image/jpg", "image/pjpeg"}},
	{"image/gif", []string{"image/gif"}},
	{"image/bmp", []string{"image/bmp", "image/x-bmp", "image/x-ms-bmp"}},
	{"image/tiff", []string{"image/tiff", "image/tif"}},
	{"image/webp", []string{"image/webp"}},
}

// textFormats are the text targets preferred in order. Requests are always
// made against a target the owner explicitly advertised, never through an
// untyped read, whose implicit fallback varies by tool version and can hand
// back raw image bytes.
var textFormats = []string{
	"text/plain;charset=utf-8",
	"UTF8_STRING",
	"text/plain",
	"STRING",
	"TEXT",
}

// chooseImageTarget returns (exact advertised name, canonical encoding) of the
// best image format in offered, mirroring the Windows "image wins over text"
// preference.
func chooseImageTarget(set map[string]bool) (exact, canonical string, ok bool) {
	for _, f := range imageFormats {
		for _, name := range f.names {
			if set[name] {
				return name, f.canonical, true
			}
		}
	}
	return "", "", false
}

// chooseTextTarget returns the best advertised text target name, or "".
func chooseTextTarget(set map[string]bool) string {
	for _, t := range textFormats {
		if set[t] {
			return t
		}
	}
	return ""
}

// snapshotFromTargets builds the shared Content from the offered target list,
// reading content through fetch (exact target name in, raw bytes out).
//  1. copied file lists are ignored;
//  2. an image target wins over text and non-PNG encodings are converted;
//  3. only with no decodable image does text get shared.
func snapshotFromTargets(offered []string, fetch func(mime string) ([]byte, error)) (Content, error) {
	set := offerSet(offered)
	if hasAny(set, fileTargets...) {
		return Content{}, nil
	}
	if exact, canonical, ok := chooseImageTarget(set); ok {
		data, err := fetch(exact)
		switch {
		case err == nil:
			pngData, perr := toPNG(data, canonical)
			if perr == nil {
				return Content{Kind: KindImage, PNG: pngData}, nil
			}
			// The bytes are not decodable as this image type: fall back to
			// text below rather than dropping the copy entirely.
		case !errors.Is(err, errNoTarget):
			return Content{}, err
		}
	}
	if t := chooseTextTarget(set); t != "" {
		data, err := fetch(t)
		if err != nil {
			if !errors.Is(err, errNoTarget) {
				return Content{}, err
			}
			return Content{}, nil
		}
		if len(data) == 0 {
			return Content{}, nil
		}
		text := string(data)
		if IsFileArtifact(text) {
			return Content{}, nil
		}
		return Content{Kind: KindText, Text: text}, nil
	}
	return Content{}, nil
}

// --- image conversion --------------------------------------------------------

const maxPixels = int64(1) << 26 // decode guard against absurd dimensions

type imageCodec struct {
	config func(io.Reader) (image.Config, error)
	decode func(io.Reader) (image.Image, error)
}

var codecs = map[string]imageCodec{
	"image/png":  {png.DecodeConfig, png.Decode},
	"image/jpeg": {jpeg.DecodeConfig, jpeg.Decode},
	"image/gif":  {gif.DecodeConfig, gif.Decode},
	"image/bmp":  {bmp.DecodeConfig, bmp.Decode},
	"image/tiff": {tiff.DecodeConfig, tiff.Decode},
	"image/webp": {webp.DecodeConfig, webp.Decode},
}

// codecOrder is the order in which decoders are tried when sniffing the
// content because the advertised MIME type turned out to be wrong.
var codecOrder = []string{"image/png", "image/jpeg", "image/gif", "image/bmp", "image/tiff", "image/webp"}

// toPNG normalizes clipboard image bytes to PNG: the canonical share format.
// PNG input is passed through untouched when valid (keeps bytes — and hence
// digests — stable across platforms); anything else is decoded and re-encoded.
func toPNG(data []byte, canonical string) ([]byte, error) {
	if canonical == "image/png" && validPNG(data) {
		return data, nil
	}
	img, err := decodeImage(data, canonical)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("clipboard: encode png: %w", err)
	}
	return buf.Bytes(), nil
}

func decodeImage(data []byte, canonical string) (image.Image, error) {
	if c, ok := codecs[canonical]; ok {
		if img, err := decodeBounded(c, data); err == nil {
			return img, nil
		}
	}
	// The advertised type may be wrong; fall back to sniffing the bytes.
	for _, name := range codecOrder {
		if img, err := decodeBounded(codecs[name], data); err == nil {
			return img, nil
		}
	}
	return nil, errors.New("clipboard: unsupported or corrupt image data")
}

func decodeBounded(c imageCodec, data []byte) (image.Image, error) {
	cfg, err := c.config(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return nil, fmt.Errorf("clipboard: unreasonable image dimensions %dx%d", cfg.Width, cfg.Height)
	}
	return c.decode(bytes.NewReader(data))
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
	if int64(cfg.Width)*int64(cfg.Height) > maxPixels { // guard against absurd dimensions
		return false
	}
	return true
}
