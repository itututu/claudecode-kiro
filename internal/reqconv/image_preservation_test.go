package reqconv

import (
	"bytes"
	"encoding/base64"
	"encoding/json/v2"
	"image"
	"image/color"
	"image/png"
	"reflect"
	"testing"

	"github.com/d-kuro/kirocc/internal/anthropic"
	"github.com/d-kuro/kirocc/internal/kiroproto"
)

func imageFixture(t *testing.T, c color.NRGBA) anthropic.ContentBlock {
	t.Helper()
	im := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	for y := range 2 {
		for x := range 2 {
			im.SetNRGBA(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return anthropic.ContentBlock{Type: "image", Source: &anthropic.ImageSource{Type: "base64", MediaType: "image/png", Data: base64.StdEncoding.EncodeToString(b.Bytes())}}
}

func TestBuildPayload_PreservesImages(t *testing.T) {
	first := imageFixture(t, color.NRGBA{R: 255, A: 255})
	second := imageFixture(t, color.NRGBA{B: 255, A: 255})
	text := func(s string) anthropic.Message {
		return anthropic.Message{Role: "user", Content: anthropic.MessageContent{Text: s}}
	}
	assistant := anthropic.Message{Role: "assistant", Content: anthropic.MessageContent{Text: "I will inspect it."}}
	imageTurn := anthropic.Message{Role: "user", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{first, second, {Type: "text", Text: "Attached images"}}}}
	toolCall := anthropic.Message{Role: "assistant", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{{Type: "tool_use", ID: "read1", Name: "Read", Input: map[string]any{"file_path": "image.png"}}}}}
	result := anthropic.Message{Role: "user", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{{Type: "tool_result", ToolUseID: "read1", IsError: true, Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{{Type: "text", Text: "Image loaded"}, first, second}}}}}}
	imageOnlyResult := result
	imageOnlyResult.Content.Blocks = []anthropic.ContentBlock{{Type: "tool_result", ToolUseID: "read1", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{first, second}}}}
	tools := []anthropic.Tool{{Name: "Read", InputSchema: map[string]any{"type": "object"}}}
	for _, tc := range []struct {
		name         string
		messages     []anthropic.Message
		tools        []anthropic.Tool
		historyIndex int // -1 means images belong to currentMessage
		system       bool
	}{
		{"current", []anthropic.Message{imageTurn}, nil, -1, false},
		{"followup", []anthropic.Message{imageTurn, assistant, text("Describe them")}, nil, 0, false},
		{"tool_round", []anthropic.Message{imageTurn, toolCall, {Role: "user", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{{Type: "tool_result", ToolUseID: "read1", Content: anthropic.MessageContent{Text: "done"}}}}}}, tools, 0, false},
		{"nested_result", []anthropic.Message{text("Read image.png"), toolCall, result}, tools, -1, false},
		{"nested_result_without_tools", []anthropic.Message{text("Read image.png"), toolCall, result}, nil, -1, false},
		{"orphan_result", []anthropic.Message{result}, tools, -1, false},
		{"image_only_result", []anthropic.Message{text("Read image.png"), toolCall, imageOnlyResult}, tools, -1, false},
		{"result_in_history", []anthropic.Message{text("Read image.png"), toolCall, result, assistant, text("Describe them again")}, tools, 2, false},
		{"system_history", []anthropic.Message{imageTurn, assistant, text("Describe them")}, nil, 2, true},
		{"assistant_last", []anthropic.Message{imageTurn, assistant}, nil, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &anthropic.Request{Model: "claude-opus-5-5", Messages: tc.messages, Tools: tc.tools}
			if tc.system {
				req.System = anthropic.SystemPrompt{Text: "Be helpful"}
			}
			before, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			payload, _, err := BuildPayload(req, BuildOptions{ModelID: "claude-opus-5.5"})
			if err != nil {
				t.Fatal(err)
			}
			wire, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			// Decode the serialized wire into the image-capable current type so
			// omission by the history type cannot be hidden by struct-only assertions.
			var decoded struct {
				State struct {
					Current kiroproto.CurrentMessage `json:"currentMessage"`
					History []struct {
						User *kiroproto.UserInputMessage `json:"userInputMessage"`
					} `json:"history"`
				} `json:"conversationState"`
			}
			if err := json.Unmarshal(wire, &decoded); err != nil {
				t.Fatal(err)
			}
			owner := &decoded.State.Current.UserInputMessage
			if tc.historyIndex >= 0 {
				owner = decoded.State.History[tc.historyIndex].User
			}
			want := []kiroproto.Image{{Format: "png", Source: kiroproto.ImageSource{Bytes: first.Source.Data}}, {Format: "png", Source: kiroproto.ImageSource{Bytes: second.Source.Data}}}
			if owner == nil || !reflect.DeepEqual(owner.Images, want) {
				t.Fatalf("images missing or out of order on owning turn; history index=%d", tc.historyIndex)
			}
			for _, b := range []anthropic.ContentBlock{first, second} {
				if n := bytes.Count(wire, []byte(b.Source.Data)); n != 1 {
					t.Fatalf("image serialized %d times, want once", n)
				}
			}
			after, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("input request was mutated")
			}
			if tc.name == "nested_result" || tc.name == "result_in_history" {
				tr := owner.UserInputMessageContext.ToolResults[0]
				if tr.ToolUseID != "read1" || tr.Status != "error" || tr.Content[0].JSON["stdout"] != "Image loaded" {
					t.Fatalf("tool result changed: %+v", tr)
				}
			}
			if tc.name == "image_only_result" {
				tr := owner.UserInputMessageContext.ToolResults[0]
				if tr.Content[0].JSON["stdout"] != "(image attached)" {
					t.Fatal("image-only result must describe its attachment rather than claim it is empty")
				}
			}
		})
	}
}

func TestNormalize_ImageToolResultsIsRepeatable(t *testing.T) {
	b := imageFixture(t, color.NRGBA{G: 255, A: 255})
	messages := []anthropic.Message{{Role: "user", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{{Type: "tool_result", ToolUseID: "missing", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{b}}}}}}}
	for _, hasTools := range []bool{false, true} {
		first := Normalize(messages, hasTools)
		second := Normalize(first, hasTools)
		if len(ExtractImages(first[0].Content)) != 1 || !reflect.DeepEqual(first, second) {
			t.Fatalf("normalization dropped or duplicated tool image (tools=%t)", hasTools)
		}
	}
}

func TestBuildPayload_ImagesStayOnSeparateTurns(t *testing.T) {
	first := imageFixture(t, color.NRGBA{R: 255, A: 255})
	second := imageFixture(t, color.NRGBA{B: 255, A: 255})
	req := &anthropic.Request{Messages: []anthropic.Message{
		{Role: "user", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{first}}},
		{Role: "assistant", Content: anthropic.MessageContent{Text: "Another image?"}},
		{Role: "user", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{second}}},
	}}
	payload, _, err := BuildPayload(req, BuildOptions{ModelID: "claude-opus-5.5"})
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var decoded kiroproto.Payload
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	historyImages := decoded.ConversationState.History[0].UserInputMessage.Images
	currentImages := decoded.ConversationState.CurrentMessage.UserInputMessage.Images
	if len(historyImages) != 1 || historyImages[0].Source.Bytes != first.Source.Data ||
		len(currentImages) != 1 || currentImages[0].Source.Bytes != second.Source.Data {
		t.Fatal("images moved away from their original user turns")
	}
	for _, b := range []anthropic.ContentBlock{first, second} {
		if n := bytes.Count(wire, []byte(b.Source.Data)); n != 1 {
			t.Fatalf("image serialized %d times, want once", n)
		}
	}
}
