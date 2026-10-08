package tui

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/masato25/rurushu-go/provider"
)

func writePNG(t *testing.T, path string, width, height int, noisy bool) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	state := uint32(0x9e3779b9)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			if noisy {
				state ^= state << 13
				state ^= state >> 17
				state ^= state << 5
				img.SetRGBA(x, y, color.RGBA{R: uint8(state), G: uint8(state >> 8), B: uint8(state >> 16), A: 255})
			} else {
				img.SetRGBA(x, y, color.RGBA{R: 30, G: 90, B: 150, A: 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareImageAttachmentKeepsSmallPNG(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "small.png")
	writePNG(t, path, 64, 32, false)

	attachment, err := prepareImageAttachment("small.png", root)
	if err != nil {
		t.Fatal(err)
	}
	if attachment.Optimized {
		t.Fatal("small PNG was optimized unexpectedly")
	}
	if attachment.MIME != "image/png" || attachment.Width != 64 || attachment.Height != 32 {
		t.Fatalf("attachment=%+v", attachment)
	}
	if !strings.HasPrefix(attachment.DataURL, "data:image/png;base64,") {
		t.Fatalf("unexpected data URL: %.32s", attachment.DataURL)
	}
}

func TestPrepareImageAttachmentDownsizesLargeImage(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "large.png")
	writePNG(t, path, 2400, 1200, false)

	attachment, err := prepareImageAttachment(path, root)
	if err != nil {
		t.Fatal(err)
	}
	if !attachment.Optimized {
		t.Fatal("large image was not optimized")
	}
	if attachment.Width > maxImageDimension || attachment.Height > maxImageDimension {
		t.Fatalf("image was not dimension-bounded: %dx%d", attachment.Width, attachment.Height)
	}
	if attachment.Width != maxImageDimension || attachment.Height != 784 {
		t.Fatalf("unexpected resized dimensions: %dx%d", attachment.Width, attachment.Height)
	}
	if attachment.Bytes > maxImagePayloadBytes {
		t.Fatalf("payload=%d exceeds limit=%d", attachment.Bytes, maxImagePayloadBytes)
	}
}

func TestPrepareImageAttachmentCompressesOversizedPayload(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "noisy.png")
	writePNG(t, path, 1200, 1200, true)

	attachment, err := prepareImageAttachment(path, root)
	if err != nil {
		t.Fatal(err)
	}
	if !attachment.Optimized {
		t.Fatal("oversized PNG was not optimized")
	}
	if attachment.Bytes > maxImagePayloadBytes {
		t.Fatalf("payload=%d exceeds limit=%d", attachment.Bytes, maxImagePayloadBytes)
	}
	if attachment.MIME != "image/jpeg" && attachment.MIME != "image/png" {
		t.Fatalf("unexpected optimized MIME %q", attachment.MIME)
	}
}

type attachmentStreamer struct {
	req provider.CompletionRequest
}

func (s *attachmentStreamer) Stream(_ context.Context, req provider.CompletionRequest) (<-chan provider.StreamEvent, error) {
	s.req = req
	ch := make(chan provider.StreamEvent, 1)
	ch <- provider.StreamEvent{Type: provider.EventDone}
	close(ch)
	return ch, nil
}

func TestAttachSlashCommandSendsImageWithNextPrompt(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "screen.png")
	writePNG(t, path, 80, 40, false)
	streamer := &attachmentStreamer{}
	m := NewWithStreamer("vision-model", "openai-compatible", streamer)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 28})

	if cmd := submitSlashTest(t, m, "/attach "+path); cmd != nil {
		t.Fatal("/attach unexpectedly started async work")
	}
	if len(m.pendingAttachments) != 1 {
		t.Fatalf("pending attachments=%d", len(m.pendingAttachments))
	}
	m.composer.SetValue("what is shown here?")
	_, cmd := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if cmd == nil {
		t.Fatal("prompt did not start stream")
	}
	_ = cmd()
	if len(streamer.req.Messages) != 1 {
		t.Fatalf("request messages=%#v", streamer.req.Messages)
	}
	msg := streamer.req.Messages[0]
	if msg.Content != "what is shown here?" || len(msg.Images) != 1 || !strings.HasPrefix(msg.Images[0], "data:image/png;base64,") {
		t.Fatalf("multimodal message=%#v", msg)
	}
	if len(m.pendingAttachments) != 0 {
		t.Fatal("attachments were not consumed after send")
	}
}

func TestImageOnlyMessageCanBeSent(t *testing.T) {
	streamer := &attachmentStreamer{}
	m := NewWithStreamer("vision-model", "openai-compatible", streamer)
	m.pendingAttachments = []imageAttachment{{Name: "x.png", DataURL: "data:image/png;base64,AA==", MIME: "image/png", Width: 1, Height: 1, Bytes: 1}}
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	_, cmd := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if cmd == nil {
		t.Fatal("image-only message did not start stream")
	}
	_ = cmd()
	if len(streamer.req.Messages) != 1 || streamer.req.Messages[0].Content != "" || len(streamer.req.Messages[0].Images) != 1 {
		t.Fatalf("image-only request=%#v", streamer.req.Messages)
	}
}

func TestCompletionRequestBoundsRepeatedVisionHistory(t *testing.T) {
	m := New("vision-model", "openai-compatible")
	m.history = []provider.Message{
		{Role: provider.RoleUser, Content: "old", Images: []string{"old-1", "old-2"}},
		{Role: provider.RoleAssistant, Content: "old answer"},
		{Role: provider.RoleUser, Content: "middle", Images: []string{"middle-1", "middle-2"}},
		{Role: provider.RoleAssistant, Content: "middle answer"},
		{Role: provider.RoleUser, Content: "latest", Images: []string{"latest-1", "latest-2"}},
	}

	req := m.completionRequest()
	if len(req.Messages[4].Images) != 2 || len(req.Messages[2].Images) != 2 || len(req.Messages[0].Images) != 0 {
		t.Fatalf("bounded history=%#v", req.Messages)
	}
	if !strings.Contains(req.Messages[0].Content, "omitted") {
		t.Fatalf("omission note missing: %q", req.Messages[0].Content)
	}
	if len(m.history[0].Images) != 2 {
		t.Fatal("bounding request mutated persisted history")
	}
}
