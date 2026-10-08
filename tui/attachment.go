package tui

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	stddraw "image/draw"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/arborlogic/rurushu-go/provider"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	maxPendingImages        = 4
	maxRequestHistoryImages = 4
	maxImageInputBytes      = 25 * 1024 * 1024
	maxImagePayloadBytes    = 768 * 1024
	maxPendingPayloadBytes  = 3 * 1024 * 1024
	maxImageDimension       = 1568
	maxImagePixels          = 50_000_000
)

type imageAttachment struct {
	Name           string
	DataURL        string
	MIME           string
	Width          int
	Height         int
	OriginalWidth  int
	OriginalHeight int
	Bytes          int
	OriginalBytes  int64
	Optimized      bool
}

func (m *Model) registerAttachmentSlashCommands() {
	_ = m.RegisterSlashCommand(SlashCommand{
		Name:        "attach",
		Usage:       "/attach <image-path>",
		Description: "attach an image to the next model message",
		Run: func(args string) (SlashCommandResult, error) {
			if len(m.pendingAttachments) >= maxPendingImages {
				return SlashCommandResult{}, fmt.Errorf("at most %d images can be attached to one message", maxPendingImages)
			}
			attachment, err := prepareImageAttachment(args, m.attachmentBaseDir())
			if err != nil {
				return SlashCommandResult{}, err
			}
			if m.pendingAttachmentBytes()+attachment.Bytes > maxPendingPayloadBytes {
				return SlashCommandResult{}, fmt.Errorf("pending image payload would exceed %s; detach an image first", formatBytes(maxPendingPayloadBytes))
			}
			m.pendingAttachments = append(m.pendingAttachments, attachment)
			m.refreshConversation()
			return SlashCommandResult{Output: "attached " + formatAttachment(attachment)}, nil
		},
	})
	_ = m.RegisterSlashCommand(SlashCommand{
		Name:        "attachments",
		Usage:       "/attachments",
		Description: "show images queued for the next model message",
		Run: func(args string) (SlashCommandResult, error) {
			if strings.TrimSpace(args) != "" {
				return SlashCommandResult{}, fmt.Errorf("/attachments does not accept arguments")
			}
			return SlashCommandResult{Output: formatPendingAttachments(m.pendingAttachments)}, nil
		},
	})
	_ = m.RegisterSlashCommand(SlashCommand{
		Name:        "detach",
		Usage:       "/detach [index|all]",
		Description: "remove a queued image attachment",
		Run: func(args string) (SlashCommandResult, error) {
			value := strings.TrimSpace(args)
			if len(m.pendingAttachments) == 0 {
				return SlashCommandResult{Output: "No pending image attachments."}, nil
			}
			if value == "" {
				items := make([]selectionPickerItem, 0, len(m.pendingAttachments))
				for i, attachment := range m.pendingAttachments {
					index := i
					label := fmt.Sprintf("#%d  %s", i+1, formatAttachment(attachment))
					items = append(items, selectionPickerItem{
						Label:   label,
						Command: fmt.Sprintf("/detach %d", i+1),
						Run: func() (string, error) {
							return m.detachAttachment(index)
						},
					})
				}
				return SlashCommandResult{picker: &selectionPicker{Title: "detach image", Items: items}}, nil
			}
			if strings.EqualFold(value, "all") {
				count := len(m.pendingAttachments)
				m.pendingAttachments = nil
				m.refreshConversation()
				return SlashCommandResult{Output: fmt.Sprintf("detached %d image(s)", count)}, nil
			}
			index, err := strconv.Atoi(value)
			if err != nil || index <= 0 || index > len(m.pendingAttachments) {
				return SlashCommandResult{}, fmt.Errorf("usage: /detach [index|all]")
			}
			output, err := m.detachAttachment(index - 1)
			return SlashCommandResult{Output: output}, err
		},
	})
}

func (m *Model) attachmentBaseDir() string {
	if m.sessionStore != nil && strings.TrimSpace(m.sessionStore.ProjectRoot) != "" {
		return m.sessionStore.ProjectRoot
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return cwd
}

func (m *Model) detachAttachment(index int) (string, error) {
	if index < 0 || index >= len(m.pendingAttachments) {
		return "", fmt.Errorf("attachment index is out of range")
	}
	name := m.pendingAttachments[index].Name
	m.pendingAttachments = append(m.pendingAttachments[:index], m.pendingAttachments[index+1:]...)
	m.refreshConversation()
	return "detached " + name, nil
}

func (m *Model) pendingAttachmentBytes() int {
	total := 0
	for _, attachment := range m.pendingAttachments {
		total += attachment.Bytes
	}
	return total
}

func (m *Model) takePendingImages() ([]string, []imageAttachment) {
	if len(m.pendingAttachments) == 0 {
		return nil, nil
	}
	attachments := append([]imageAttachment(nil), m.pendingAttachments...)
	images := make([]string, 0, len(attachments))
	for _, attachment := range attachments {
		images = append(images, attachment.DataURL)
	}
	m.pendingAttachments = nil
	return images, attachments
}

func prepareImageAttachment(rawPath, baseDir string) (imageAttachment, error) {
	path := cleanAttachmentPath(rawPath)
	if path == "" {
		return imageAttachment{}, fmt.Errorf("usage: /attach <image-path>")
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return imageAttachment{}, fmt.Errorf("resolve home directory: %w", err)
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}
	path = filepath.Clean(path)

	info, err := os.Stat(path)
	if err != nil {
		return imageAttachment{}, fmt.Errorf("inspect image %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return imageAttachment{}, fmt.Errorf("image path must be a regular file")
	}
	if info.Size() <= 0 {
		return imageAttachment{}, fmt.Errorf("image is empty")
	}
	if info.Size() > maxImageInputBytes {
		return imageAttachment{}, fmt.Errorf("image is %s; maximum input size is %s", formatBytes(int(info.Size())), formatBytes(maxImageInputBytes))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return imageAttachment{}, fmt.Errorf("read image %q: %w", path, err)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return imageAttachment{}, fmt.Errorf("unsupported or invalid image %q: %w", filepath.Base(path), err)
	}
	if config.Width <= 0 || config.Height <= 0 {
		return imageAttachment{}, fmt.Errorf("image has invalid dimensions")
	}
	if int64(config.Width)*int64(config.Height) > maxImagePixels {
		return imageAttachment{}, fmt.Errorf("image dimensions %dx%d exceed the safe decode limit", config.Width, config.Height)
	}
	mime := mimeForImageFormat(format)
	if mime == "" {
		return imageAttachment{}, fmt.Errorf("unsupported image format %q", format)
	}

	attachment := imageAttachment{
		Name:           filepath.Base(path),
		MIME:           mime,
		Width:          config.Width,
		Height:         config.Height,
		OriginalWidth:  config.Width,
		OriginalHeight: config.Height,
		Bytes:          len(data),
		OriginalBytes:  info.Size(),
	}

	needsResize := max(config.Width, config.Height) > maxImageDimension
	needsReencode := len(data) > maxImagePayloadBytes || (format != "jpeg" && format != "png")
	if !needsResize && !needsReencode {
		attachment.DataURL = makeDataURL(mime, data)
		return attachment, nil
	}

	decoded, decodedFormat, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return imageAttachment{}, fmt.Errorf("decode image %q: %w", filepath.Base(path), err)
	}
	working := decoded
	if needsResize {
		width, height := fitDimensions(config.Width, config.Height, maxImageDimension)
		working = resizeImage(decoded, width, height)
	}

	encoded, encodedMIME, finalImage, err := encodeImageWithinBudget(working, decodedFormat, maxImagePayloadBytes)
	if err != nil {
		return imageAttachment{}, fmt.Errorf("optimize image %q: %w", filepath.Base(path), err)
	}
	bounds := finalImage.Bounds()
	attachment.MIME = encodedMIME
	attachment.Width = bounds.Dx()
	attachment.Height = bounds.Dy()
	attachment.Bytes = len(encoded)
	attachment.Optimized = true
	attachment.DataURL = makeDataURL(encodedMIME, encoded)
	return attachment, nil
}

func cleanAttachmentPath(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
		if value[0] == '"' {
			if unquoted, err := strconv.Unquote(value); err == nil {
				return unquoted
			}
		}
		return value[1 : len(value)-1]
	}
	return value
}

func mimeForImageFormat(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "jpeg", "jpg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	default:
		return ""
	}
}

func makeDataURL(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func fitDimensions(width, height, maxDimension int) (int, int) {
	if width <= maxDimension && height <= maxDimension {
		return width, height
	}
	if width >= height {
		return maxDimension, max(1, height*maxDimension/width)
	}
	return max(1, width*maxDimension/height), maxDimension
}

func resizeImage(src image.Image, width, height int) image.Image {
	if src.Bounds().Dx() == width && src.Bounds().Dy() == height {
		return src
	}
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Over, nil)
	return dst
}

func encodeImageWithinBudget(src image.Image, sourceFormat string, maxBytes int) ([]byte, string, image.Image, error) {
	if strings.EqualFold(sourceFormat, "png") {
		var pngBuf bytes.Buffer
		if err := png.Encode(&pngBuf, src); err != nil {
			return nil, "", nil, err
		}
		if pngBuf.Len() <= maxBytes {
			return pngBuf.Bytes(), "image/png", src, nil
		}
	}

	working := src
	var smallest []byte
	var smallestImage image.Image
	for scalePass := 0; scalePass < 5; scalePass++ {
		for _, quality := range []int{85, 78, 70, 62, 55, 48} {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, flattenOnWhite(working), &jpeg.Options{Quality: quality}); err != nil {
				return nil, "", nil, err
			}
			candidate := append([]byte(nil), buf.Bytes()...)
			if len(smallest) == 0 || len(candidate) < len(smallest) {
				smallest = candidate
				smallestImage = working
			}
			if len(candidate) <= maxBytes {
				return candidate, "image/jpeg", working, nil
			}
		}
		bounds := working.Bounds()
		if bounds.Dx() <= 512 && bounds.Dy() <= 512 {
			break
		}
		width := max(1, bounds.Dx()*4/5)
		height := max(1, bounds.Dy()*4/5)
		working = resizeImage(working, width, height)
	}
	if len(smallest) == 0 {
		return nil, "", nil, fmt.Errorf("could not encode image")
	}
	if len(smallest) > maxBytes {
		return nil, "", nil, fmt.Errorf("compressed image remains %s (limit %s)", formatBytes(len(smallest)), formatBytes(maxBytes))
	}
	return smallest, "image/jpeg", smallestImage, nil
}

func flattenOnWhite(src image.Image) image.Image {
	bounds := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	stddraw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.White}, image.Point{}, stddraw.Src)
	stddraw.Draw(dst, dst.Bounds(), src, bounds.Min, stddraw.Over)
	return dst
}

func formatAttachment(attachment imageAttachment) string {
	dimensions := fmt.Sprintf("%dx%d", attachment.Width, attachment.Height)
	size := formatBytes(attachment.Bytes)
	if attachment.Optimized && (attachment.Width != attachment.OriginalWidth || attachment.Height != attachment.OriginalHeight || int64(attachment.Bytes) != attachment.OriginalBytes) {
		return fmt.Sprintf("%s · %dx%d → %s · %s → %s", attachment.Name, attachment.OriginalWidth, attachment.OriginalHeight, dimensions, formatBytes(int(attachment.OriginalBytes)), size)
	}
	return fmt.Sprintf("%s · %s · %s", attachment.Name, dimensions, size)
}

func formatPendingAttachments(attachments []imageAttachment) string {
	if len(attachments) == 0 {
		return "No pending image attachments."
	}
	var b strings.Builder
	b.WriteString("pending images")
	for i, attachment := range attachments {
		fmt.Fprintf(&b, "\n  #%d  %s", i+1, formatAttachment(attachment))
	}
	b.WriteString("\n\nThey will be sent with the next model message.")
	return b.String()
}

func formatPendingAttachmentHint(attachments []imageAttachment) string {
	if len(attachments) == 0 {
		return ""
	}
	if len(attachments) == 1 {
		return "image queued: " + formatAttachment(attachments[0]) + "  ·  /detach to remove"
	}
	return fmt.Sprintf("%d images queued · %s total  ·  /attachments to inspect", len(attachments), formatBytes(totalAttachmentBytes(attachments)))
}

func totalAttachmentBytes(attachments []imageAttachment) int {
	total := 0
	for _, attachment := range attachments {
		total += attachment.Bytes
	}
	return total
}

func formatUserMessageWithImages(content string, attachments []imageAttachment) string {
	content = strings.TrimSpace(content)
	if len(attachments) == 0 {
		return content
	}
	var b strings.Builder
	b.WriteString(content)
	if b.Len() > 0 {
		b.WriteString("\n\n")
	}
	for i, attachment := range attachments {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "[image: %s · %dx%d]", attachment.Name, attachment.Width, attachment.Height)
	}
	return b.String()
}

func formatPersistedUserMessage(content string, imageCount int) string {
	content = strings.TrimSpace(content)
	if imageCount <= 0 {
		return content
	}
	var b strings.Builder
	b.WriteString(content)
	if b.Len() > 0 {
		b.WriteString("\n\n")
	}
	if imageCount == 1 {
		b.WriteString("[image attachment]")
	} else {
		fmt.Fprintf(&b, "[%d image attachments]", imageCount)
	}
	return b.String()
}

func boundVisionHistory(messages []provider.Message, maxImages int) []provider.Message {
	cloned := cloneProviderMessages(messages)
	if maxImages < 0 {
		maxImages = 0
	}
	remaining := maxImages
	for i := len(cloned) - 1; i >= 0; i-- {
		if len(cloned[i].Images) == 0 {
			continue
		}
		keep := min(remaining, len(cloned[i].Images))
		omitted := len(cloned[i].Images) - keep
		if keep == 0 {
			cloned[i].Images = nil
		} else {
			cloned[i].Images = append([]string(nil), cloned[i].Images[:keep]...)
		}
		remaining -= keep
		if omitted > 0 {
			note := fmt.Sprintf("[%d older image attachment(s) omitted from this request to limit repeated vision-token usage]", omitted)
			if strings.TrimSpace(cloned[i].Content) == "" {
				cloned[i].Content = note
			} else {
				cloned[i].Content = strings.TrimSpace(cloned[i].Content) + "\n\n" + note
			}
		}
	}
	return cloned
}

func attachmentFromDataURL(dataURL string, index int) imageAttachment {
	attachment := imageAttachment{Name: fmt.Sprintf("rewound-image-%d", index+1), DataURL: dataURL}
	header, encoded, ok := strings.Cut(dataURL, ",")
	if !ok || !strings.HasPrefix(header, "data:image/") || !strings.Contains(header, ";base64") {
		return attachment
	}
	attachment.MIME = strings.TrimPrefix(strings.SplitN(header, ";", 2)[0], "data:")
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return attachment
	}
	attachment.Bytes = len(data)
	attachment.OriginalBytes = int64(len(data))
	if config, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		attachment.Width = config.Width
		attachment.Height = config.Height
		attachment.OriginalWidth = config.Width
		attachment.OriginalHeight = config.Height
	}
	return attachment
}

func formatBytes(value int) string {
	if value < 1024 {
		return fmt.Sprintf("%d B", value)
	}
	if value < 1024*1024 {
		return fmt.Sprintf("%.0f KiB", float64(value)/1024)
	}
	return fmt.Sprintf("%.1f MiB", float64(value)/(1024*1024))
}
