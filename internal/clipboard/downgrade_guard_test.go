package clipboard

import (
	"errors"
	"testing"
)

// TestApplyRemoteKeepsLocalRichOverPlainDowngrade covers the "my formatted copy
// turned into plain text" symptom: a peer that lost the HTML somewhere (a VM /
// cloud-desktop clipboard agent that only mirrors plain text, or a source
// application that never offered HTML) sends the same text without formatting.
// Applying it would silently replace the local formatted copy, so it must be
// refused instead.
func TestApplyRemoteKeepsLocalRichOverPlainDowngrade(t *testing.T) {
	rich := Content{Kind: KindText, Text: "hello world", HTML: "<b>hello</b> world"}
	plain := Content{Kind: KindText, Text: "hello world"}

	t.Run("plain rendition of the same text is refused", func(t *testing.T) {
		b := &fakeBackend{content: rich}
		w := NewWatcher(b, func(Content) {})
		if err := w.ApplyRemote(plain); !errors.Is(err, ErrKeptLocalRich) {
			t.Fatalf("ApplyRemote(plain downgrade) = %v, want ErrKeptLocalRich", err)
		}
		got, _ := b.Snapshot()
		if got.HTML == "" {
			t.Fatalf("local rich text was downgraded to plain: %+v", got)
		}
	})

	t.Run("plain text of different content is still applied", func(t *testing.T) {
		b := &fakeBackend{content: rich}
		w := NewWatcher(b, func(Content) {})
		other := Content{Kind: KindText, Text: "something else"}
		if err := w.ApplyRemote(other); err != nil {
			t.Fatalf("ApplyRemote(other plain text) = %v, want nil", err)
		}
		got, _ := b.Snapshot()
		if got.Text != "something else" || got.HTML != "" {
			t.Fatalf("different plain text was not applied: %+v", got)
		}
	})

	t.Run("incoming rich text is still applied", func(t *testing.T) {
		b := &fakeBackend{content: Content{Kind: KindText, Text: "old", HTML: "<b>old</b>"}}
		w := NewWatcher(b, func(Content) {})
		incoming := Content{Kind: KindText, Text: "new", HTML: "<i>new</i>"}
		if err := w.ApplyRemote(incoming); err != nil {
			t.Fatalf("ApplyRemote(rich) = %v, want nil", err)
		}
		got, _ := b.Snapshot()
		if got.Text != "new" || got.HTML != "<i>new</i>" {
			t.Fatalf("rich content was not applied: %+v", got)
		}
	})

	t.Run("plain text replaces plain text", func(t *testing.T) {
		b := &fakeBackend{content: Content{Kind: KindText, Text: "old plain"}}
		w := NewWatcher(b, func(Content) {})
		if err := w.ApplyRemote(Content{Kind: KindText, Text: "old plain"}); err != nil {
			t.Fatalf("ApplyRemote(plain over plain) = %v, want nil", err)
		}
	})

	t.Run("image is unaffected", func(t *testing.T) {
		png := encodePng(t, testRGBAImage())
		b := &fakeBackend{content: rich}
		w := NewWatcher(b, func(Content) {})
		if err := w.ApplyRemote(Content{Kind: KindImage, PNG: png}); err != nil {
			t.Fatalf("ApplyRemote(image) = %v, want nil", err)
		}
		got, _ := b.Snapshot()
		if got.Kind != KindImage {
			t.Fatalf("image was not applied: %+v", got)
		}
	})
}
