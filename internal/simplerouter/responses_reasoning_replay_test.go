package simplerouter

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestOpenRouterReasoningSignatureRoundTrip(t *testing.T) {
	for _, original := range []string{
		`{"type":"reasoning","id":"rs_visible","summary":[],"content":[{"type":"reasoning_text","text":"Check the cache → invalidate the entry."}],"signature":"opaque+/=signature","format":"anthropic-claude-v1"}`,
		`{"type":"reasoning","id":"rs_omitted","summary":[],"content":[],"signature":"opaque+/=omitted","format":"anthropic-claude-v1","encrypted_content":null}`,
		`{"type":"reasoning","id":"rs_both","summary":[],"content":null,"signature":"opaque+/=signature","format":"google-gemini-v1","encrypted_content":"native-encrypted-state"}`,
	} {
		var want map[string]any
		if err := json.Unmarshal([]byte(original), &want); err != nil {
			t.Fatal(err)
		}
		t.Run(want["id"].(string), func(t *testing.T) {
			captured := make(chan map[string]any, 2)
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					http.Error(writer, err.Error(), http.StatusBadRequest)
					return
				}
				captured <- body
				writer.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(writer, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":%s}\n\n", original)
				fmt.Fprintf(writer, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[%s]}}\n\n", original)
			}))
			defer upstream.Close()
			proxy := httptest.NewServer(newResponsesPassthroughProxy(upstream.URL, "anthropic/claude-opus-5.5", upstream.Client(), openRouterResponsesOptions("anthropic/claude-opus-5.5", "")))
			defer proxy.Close()

			response, err := http.Post(proxy.URL+"/responses", "application/json", strings.NewReader(`{"input":[],"stream":true}`))
			if err != nil {
				t.Fatal(err)
			}
			stream, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			<-captured
			var items []json.RawMessage
			for _, line := range strings.Split(string(stream), "\n") {
				payload, found := strings.CutPrefix(line, "data: ")
				if !found {
					continue
				}
				var event struct {
					Item     json.RawMessage `json:"item"`
					Response struct {
						Output []json.RawMessage `json:"output"`
					} `json:"response"`
				}
				if err := json.Unmarshal([]byte(payload), &event); err != nil {
					t.Fatal(err)
				}
				if len(event.Item) > 0 {
					items = append(items, event.Item)
				}
				items = append(items, event.Response.Output...)
			}
			if len(items) != 2 {
				t.Fatalf("reasoning items = %d, want 2", len(items))
			}
			if !bytes.Equal(items[0], items[1]) {
				t.Fatal("output_item.done and response.completed contain different reasoning")
			}
			var persisted struct {
				Type             string          `json:"type"`
				ID               string          `json:"id"`
				Summary          json.RawMessage `json:"summary"`
				Content          json.RawMessage `json:"content"`
				EncryptedContent *string         `json:"encrypted_content"`
			}
			if err := json.Unmarshal(items[0], &persisted); err != nil {
				t.Fatal(err)
			}
			if persisted.EncryptedContent == nil || *persisted.EncryptedContent == "" {
				t.Fatal("reasoning signature is lost when Codex persists the item")
			}
			replay, err := json.Marshal(map[string]any{"input": []any{persisted}, "stream": true})
			if err != nil {
				t.Fatal(err)
			}
			response, err = http.Post(proxy.URL+"/responses", "application/json", bytes.NewReader(replay))
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("replay status = %d", response.StatusCode)
			}
			request := <-captured
			got := request["input"].([]any)[0]
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("replayed reasoning = %#v, want %#v", got, want)
			}
		})
	}
}

func TestResponsesReasoningSignaturesAcrossStreamEvents(t *testing.T) {
	item := `{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"Visible summary."}],"signature":"opaque+/=signature","format":"anthropic-claude-v1"}`
	for _, eventType := range []string{
		"response.output_item.added", "response.output_item.done",
		"response.created", "response.in_progress", "response.completed", "response.incomplete", "response.failed",
	} {
		t.Run(eventType, func(t *testing.T) {
			field := `"item":` + item
			if !strings.HasPrefix(eventType, "response.output_item.") {
				field = `"response":{"usage":{"output_tokens":42},"output":[` + item + `,{"type":"message","content":[]}]}`
			}
			block := []byte("id: event-1\nevent: " + eventType + "\ndata: {\"type\":\"" + eventType + "\"," + field + "}\n\n")
			for _, original := range [][]byte{block, bytes.TrimSuffix(block, []byte("\n\n"))} {
				proxy := newResponsesPassthroughProxy("", "anthropic/claude-opus-5.5", nil, openRouterResponsesOptions("anthropic/claude-opus-5.5", ""))
				writer := httptest.NewRecorder()
				response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(bytes.NewReader(original))}
				proxy.relayResponsesStream(writer, response, responsesToolTranslation{})
				got := writer.Body.Bytes()
				if !bytes.HasPrefix(got, []byte("id: event-1\nevent: "+eventType+"\n")) {
					t.Fatal("SSE metadata changed")
				}
				if !bytes.Contains(got, []byte(responsesReasoningReplayPrefix)) || !bytes.Contains(got, []byte("Visible summary.")) {
					t.Fatal("stream lost reasoning replay state or visible text")
				}
				if !strings.HasPrefix(eventType, "response.output_item.") && !bytes.Contains(got, []byte(`"output_tokens":42`)) {
					t.Fatal("stream lost response usage")
				}
				if second := preserveResponsesReasoningSignatures(got); !bytes.Equal(second, got) {
					t.Fatal("reasoning replay state was wrapped twice")
				}
			}
		})
	}
}

func TestResponsesReasoningSignaturesLeaveOtherDataUnchanged(t *testing.T) {
	for _, block := range []string{
		"data: [DONE]\n\n",
		": keepalive\n\n",
		"data: invalid\n\n",
		"data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"Visible text.\"}\n\n",
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"summary\":[],\"encrypted_content\":\"native-state\"}}\n\n",
		"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\",\"signature\":\"\",\"format\":\"anthropic-claude-v1\"}}\n\n",
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"signature\":\"unrelated\"}}\n\n",
	} {
		if got := preserveResponsesReasoningSignatures([]byte(block)); string(got) != block {
			t.Fatalf("unrelated SSE block changed: %q", got)
		}
	}
	input := json.RawMessage(`[{"type":"reasoning","encrypted_content":"native-state","summary":[]},{"type":"reasoning","encrypted_content":null,"content":[]},{"type":"message","content":[]}]`)
	request := map[string]json.RawMessage{"input": input}
	if err := restoreResponsesReasoningSignatures(request); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(request["input"], input) {
		t.Fatal("native encrypted reasoning or legacy unsigned history changed")
	}
	block := []byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"signature\":\"unrelated\"}}\n\n")
	proxy := newResponsesPassthroughProxy("", "other-model", nil, responsesPassthroughOptions{})
	writer := httptest.NewRecorder()
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(block))}
	proxy.relayResponsesStream(writer, response, responsesToolTranslation{})
	if !bytes.Equal(writer.Body.Bytes(), block) {
		t.Fatal("non-OpenRouter stream changed")
	}
}

func TestResponsesReasoningSignaturesRejectCorruptReplay(t *testing.T) {
	for _, encoded := range []string{
		"not-base64!",
		base64.RawStdEncoding.EncodeToString([]byte(`not-json`)),
		base64.RawStdEncoding.EncodeToString([]byte(`{}`)),
		base64.RawStdEncoding.EncodeToString([]byte(`{"signature":null}`)),
	} {
		requestBody, err := json.Marshal(map[string]any{"input": []any{map[string]any{
			"type": "reasoning", "encrypted_content": responsesReasoningReplayPrefix + encoded,
		}}})
		if err != nil {
			t.Fatal(err)
		}
		proxy := newResponsesPassthroughProxy("http://unused.invalid", "anthropic/claude-opus-5.5", nil, openRouterResponsesOptions("anthropic/claude-opus-5.5", ""))
		writer := httptest.NewRecorder()
		proxy.ServeHTTP(writer, httptest.NewRequest(http.MethodPost, "/responses", bytes.NewReader(requestBody)))
		if writer.Code != http.StatusBadRequest || !strings.Contains(writer.Body.String(), "invalid reasoning replay state at input[0]") {
			t.Fatalf("corrupt replay response = %d %s", writer.Code, writer.Body.String())
		}
	}
}
