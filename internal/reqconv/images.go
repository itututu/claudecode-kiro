package reqconv

import (
	"log/slog"
	"strings"

	"github.com/d-kuro/kirocc/internal/anthropic"
	"github.com/d-kuro/kirocc/internal/kiroproto"
)

// ExtractImages extracts image blocks from message content and converts to Kiro format.
// URL-based images are skipped with a warning log.
func ExtractImages(content anthropic.MessageContent) []kiroproto.Image {
	if content.IsString() {
		return nil
	}
	var images []kiroproto.Image
	for _, b := range content.Blocks {
		if img, ok := convertImageBlock(b); ok {
			images = append(images, img)
		}
	}
	return images
}

func convertImageBlock(b anthropic.ContentBlock) (kiroproto.Image, bool) {
	if b.Type != anthropic.BlockTypeImage || b.Source == nil {
		return kiroproto.Image{}, false
	}
	if b.Source.Type != "base64" {
		slog.Warn("skipping non-base64 image source type", "type", b.Source.Type)
		return kiroproto.Image{}, false
	}
	format := b.Source.MediaType
	if idx := strings.LastIndex(format, "/"); idx >= 0 {
		format = format[idx+1:]
	}
	return kiroproto.Image{Format: format, Source: kiroproto.ImageSource{Bytes: b.Source.Data}}, true
}

// promoteToolResultImages moves nested tool images onto their owning user turn
// before tool-result textualization can discard them. Input slices stay intact.
func promoteToolResultImages(msgs []anthropic.Message) []anthropic.Message {
	result := make([]anthropic.Message, 0, len(msgs))
	for _, msg := range msgs {
		if msg.Role != "user" || msg.Content.IsString() {
			result = append(result, msg)
			continue
		}
		var blocks []anthropic.ContentBlock
		changed := false
		for _, b := range msg.Content.Blocks {
			var images, remaining []anthropic.ContentBlock
			if b.IsToolResult() && !b.Content.IsString() {
				for _, child := range b.Content.Blocks {
					if child.Type == anthropic.BlockTypeImage {
						images = append(images, child)
					} else {
						remaining = append(remaining, child)
					}
				}
			}
			if len(images) > 0 {
				if len(remaining) == 0 {
					remaining = []anthropic.ContentBlock{{Type: anthropic.BlockTypeText, Text: "(image attached)"}}
				}
				b.Content = anthropic.MessageContent{Blocks: remaining}
				changed = true
			}
			blocks = append(blocks, b)
			blocks = append(blocks, images...)
		}
		if changed {
			msg.Content = anthropic.MessageContent{Blocks: blocks}
		}
		result = append(result, msg)
	}
	return result
}
