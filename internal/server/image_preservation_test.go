package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestE2E_ImageHistoryAndToolResults(t *testing.T) {
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	data := base64.StdEncoding.EncodeToString(pngBytes.Bytes())
	imageJSON := fmt.Sprintf(`{"type":"image","source":{"type":"base64","media_type":"image/png","data":%q}}`, data)
	for _, stream := range []bool{false, true} {
		for _, history := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/history=%t", stream, history), func(t *testing.T) {
				client := &capturingClient{events: []any{"assistantResponseEvent", mustJSON(map[string]string{"content": "fixture response"})}}
				srv := newE2EServer(t, client)
				defer srv.Close()
				messages := `{"role":"user","content":"Read image.png"},{"role":"assistant","content":[{"type":"tool_use","id":"read1","name":"Read","input":{"file_path":"image.png"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"read1","content":[{"type":"text","text":"loaded"},` + imageJSON + `]}]}`
				if history {
					messages += `,{"role":"assistant","content":"I will inspect it"},{"role":"user","content":"Describe it again"}`
				}
				body := fmt.Sprintf(`{"model":"claude-opus-5-5","stream":%t,"messages":[%s],"tools":[{"name":"Read","input_schema":{"type":"object"}}]}`, stream, messages)
				resp := postMessages(t, srv.URL, body)
				defer func() { _ = resp.Body.Close() }()
				requireStatus(t, resp, 200)
				responseBody, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				if stream && !strings.Contains(string(responseBody), "event: message_stop") {
					t.Fatal("stream did not complete")
				}
				requireCaptured(t, client)
				wire, err := json.Marshal(client.captured)
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Count(wire, []byte(data)) != 1 {
					t.Fatal("wire payload dropped or duplicated image")
				}
				if history {
					if len(client.captured.ConversationState.History[2].UserInputMessage.Images) != 1 {
						t.Fatal("image is not on its owning history turn")
					}
				} else if len(client.captured.ConversationState.CurrentMessage.UserInputMessage.Images) != 1 {
					t.Fatal("tool image is not on current turn")
				}
				request, err := http.NewRequest("POST", srv.URL+"/v1/messages/count_tokens", strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				countResp, err := http.DefaultClient.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = countResp.Body.Close() }()
				requireStatus(t, countResp, 200)
			})
		}
	}
}
