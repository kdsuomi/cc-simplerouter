package simplerouter

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

var errStreamAborted = errors.New("upstream stream aborted")

// chatToolArguments accepts both encodings used by OpenAI-compatible
// providers: a JSON-encoded string or a bare JSON value.
type chatToolArguments string

func (a *chatToolArguments) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*a = ""
		return nil
	}
	var value string
	if err := json.Unmarshal(trimmed, &value); err == nil {
		*a = chatToolArguments(value)
		return nil
	}
	if !json.Valid(trimmed) {
		return fmt.Errorf("invalid tool arguments JSON")
	}
	*a = chatToolArguments(string(trimmed))
	return nil
}

type sseWriter struct {
	w     io.Writer
	flush http.Flusher
}

func (s *sseWriter) event(name string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = io.WriteString(s.w, "event: "+name+"\ndata: ")
	_, _ = s.w.Write(data)
	_, _ = io.WriteString(s.w, "\n\n")
	if s.flush != nil {
		s.flush.Flush()
	}
}

func readCompatSSE(body io.Reader, emit func(json.RawMessage) error) error {
	reader := bufio.NewReader(body)
	var data strings.Builder
	flush := func() error {
		payload := strings.TrimSpace(data.String())
		data.Reset()
		if payload == "" || payload == "[DONE]" {
			return nil
		}
		return emit(json.RawMessage(payload))
	}
	for {
		line, err := reader.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if flushErr := flush(); flushErr != nil {
				return flushErr
			}
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
		if err != nil {
			if flushErr := flush(); flushErr != nil {
				return flushErr
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func apiKeyFromRequest(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, `{"error":{"type":"api_error","message":"encode response"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func newToolUseID() string {
	var value [16]byte
	_, _ = rand.Read(value[:])
	return "toolu_" + hex.EncodeToString(value[:])
}

func newMessageID() string {
	var value [12]byte
	_, _ = rand.Read(value[:])
	return "msg_" + hex.EncodeToString(value[:])
}

func envWithout(env []string, keys ...string) []string {
	blocked := make(map[string]bool, len(keys))
	for _, key := range keys {
		blocked[strings.ToUpper(key)] = true
	}
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if ok && blocked[strings.ToUpper(key)] {
			continue
		}
		out = append(out, entry)
	}
	return out
}
