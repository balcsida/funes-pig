package funes

// Convert a PiG session (`~/.pig/agent/sessions/<munged-cwd>/<stem>.jsonl`, pi's session format)
// into a funes turns file (`<stem>.funes.jsonl`, funes's docs/funes-jsonl.md). A port of the pi
// bundle's convert.mjs in huggingface/funes-integrations: the same mapping, with `harness` "pig".
//
// `session_id` is the session file's stem, as the pi bundle keys its sessions.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	harness = "pig"
	format  = 1
)

type block struct {
	BlockType string `json:"block_type"`
	Text      string `json:"text"`
	ToolName  string `json:"tool_name,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
}

// Field order is the wire order of funes's own Turn.
type turn struct {
	Format     int     `json:"format"`
	SessionID  string  `json:"session_id"`
	Cwd        *string `json:"cwd,omitempty"`
	TurnUUID   string  `json:"turn_uuid"`
	ParentUUID *string `json:"parent_uuid,omitempty"`
	Seq        int     `json:"seq"`
	Ts         string  `json:"ts"`
	Role       string  `json:"role"`
	Blocks     []block `json:"blocks"`
	Harness    string  `json:"harness"`
}

type part struct {
	Type      string          `json:"type"`
	Text      any             `json:"text"`
	Thinking  any             `json:"thinking"`
	Name      any             `json:"name"`
	ID        any             `json:"id"`
	Arguments json.RawMessage `json:"arguments"`
}

type message struct {
	Role       any             `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolName   any             `json:"toolName"`
	ToolCallID any             `json:"toolCallId"`
}

type record struct {
	Type      any             `json:"type"`
	Cwd       any             `json:"cwd"`
	ID        any             `json:"id"`
	ParentID  any             `json:"parentId"`
	Timestamp any             `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

// isBlank tests Unicode White_Space, as Rust's `str::trim` does; Go's unicode.IsSpace is that set.
func isBlank(s string) bool { return strings.TrimSpace(s) == "" }

// str is `Value::as_str`: a string, else nothing.
func str(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func optStr(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

// hasLoneSurrogate reports a `\uD800`-`\uDFFF` escape without its pair: serde_json rejects such a
// line and funes drops it, where encoding/json would quietly substitute U+FFFD.
func hasLoneSurrogate(line string) bool {
	high := false
	for i := 0; i < len(line); i++ {
		if line[i] != '\\' {
			if high {
				return true
			}
			continue
		}
		if i+1 >= len(line) {
			break
		}
		if line[i+1] != 'u' || i+5 >= len(line) {
			if high {
				return true
			}
			i++ // an escaped character, `\\` included
			continue
		}
		var r rune
		if _, err := fmt.Sscanf(line[i+2:i+6], "%04x", &r); err != nil {
			i++
			continue
		}
		switch {
		case r >= 0xD800 && r <= 0xDBFF:
			if high {
				return true
			}
			high = true
		case r >= 0xDC00 && r <= 0xDFFF:
			if !high {
				return true
			}
			high = false
		default:
			if high {
				return true
			}
		}
		i += 5
	}
	return high
}

// readRecords is every parseable JSON object of a JSONL file, in file order.
func readRecords(path string) ([]record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var records []record
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var r record
		if json.Unmarshal([]byte(line), &r) != nil {
			continue // a partial trailing write
		}
		if strings.Contains(strings.ToLower(line), `\ud`) && hasLoneSurrogate(line) {
			continue
		}
		records = append(records, r)
	}
	return records, nil
}

func textLike(blockType string, v any) []block {
	text, _ := str(v)
	if isBlank(text) {
		return nil
	}
	return []block{{BlockType: blockType, Text: text}}
}

// flattenToolResult is a tool result's `content` (a string, or a list of `{type:"text",text}` parts)
// as one string.
func flattenToolResult(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return ""
	}
	var parts []string
	for _, item := range items {
		var p part
		if json.Unmarshal(item, &p) == nil && bytes.HasPrefix(bytes.TrimSpace(item), []byte("{")) {
			if p.Type == "text" {
				t, _ := str(p.Text)
				parts = append(parts, t)
			}
		} else if json.Unmarshal(item, &s) == nil {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n")
}

func parts(raw json.RawMessage) []part {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	var out []part
	for _, item := range items {
		var p part
		if bytes.HasPrefix(bytes.TrimSpace(item), []byte("{")) && json.Unmarshal(item, &p) == nil {
			out = append(out, p)
		}
	}
	return out
}

func toolUseBlock(p part) block {
	args := "{}"
	if len(p.Arguments) > 0 {
		var buf bytes.Buffer
		if json.Compact(&buf, p.Arguments) == nil {
			args = buf.String()
		}
	}
	b := block{BlockType: "tool_use", Text: args}
	b.ToolName, _ = str(p.Name)
	b.ToolUseID, _ = str(p.ID)
	return b
}

func blocksOf(role string, msg message) []block {
	var blocks []block
	switch role {
	case "user":
		for _, p := range parts(msg.Content) {
			if p.Type == "text" {
				blocks = append(blocks, textLike("text", p.Text)...)
			}
		}
	case "assistant":
		for _, p := range parts(msg.Content) {
			switch p.Type {
			case "thinking":
				blocks = append(blocks, textLike("thinking", p.Thinking)...)
			case "text":
				blocks = append(blocks, textLike("text", p.Text)...)
			case "toolCall":
				blocks = append(blocks, toolUseBlock(p))
			}
		}
	case "toolResult":
		text := flattenToolResult(msg.Content)
		if !isBlank(text) {
			b := block{BlockType: "tool_result", Text: text}
			b.ToolName, _ = str(msg.ToolName)
			b.ToolUseID, _ = str(msg.ToolCallID)
			blocks = append(blocks, b)
		}
	}
	return blocks
}

// timestamp is a record's `timestamp` as the format wants it: an ISO string as written, and the
// epoch milliseconds an older session wrote as the instant they name.
func timestamp(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return time.UnixMilli(int64(t)).UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return ""
}

func turnsOf(sessionPath, sessionID string) ([]turn, error) {
	records, err := readRecords(sessionPath)
	if err != nil {
		return nil, err
	}
	var cwd *string
	for _, r := range records {
		if r.Type == "session" {
			cwd = optStr(r.Cwd)
			break
		}
	}
	var turns []turn
	for _, r := range records {
		if r.Type != "message" || !bytes.HasPrefix(r.Message, []byte("{")) {
			continue
		}
		var msg message
		if json.Unmarshal(r.Message, &msg) != nil {
			continue
		}
		role, _ := str(msg.Role)
		blocks := blocksOf(role, msg)
		if len(blocks) == 0 {
			continue
		}
		if role == "toolResult" {
			role = "tool"
		}
		id, _ := str(r.ID)
		turns = append(turns, turn{
			Format: format, SessionID: sessionID, Cwd: cwd, TurnUUID: id, ParentUUID: optStr(r.ParentID),
			Seq: len(turns), Ts: timestamp(r.Timestamp), Role: role, Blocks: blocks, Harness: harness,
		})
	}
	return turns, nil
}

// convert writes one session to `<spool>/<stem>.funes.jsonl` and returns the path written.
func convert(sessionPath, spool string) (string, error) {
	stem := strings.TrimSuffix(filepath.Base(sessionPath), ".jsonl")
	out := filepath.Join(spool, stem+".funes.jsonl")
	turns, err := turnsOf(sessionPath, stem)
	if err != nil {
		return "", err
	}
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	enc.SetEscapeHTML(false)
	for _, t := range turns {
		if err := enc.Encode(t); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(spool, 0o755); err != nil {
		return "", err
	}
	// Atomic, and off `.jsonl`, so a concurrent `funes index` over the spool never lists it.
	tmp := fmt.Sprintf("%s.tmp%d", out, os.Getpid())
	if err := os.WriteFile(tmp, body.Bytes(), 0o644); err != nil {
		return "", err
	}
	// Stamped with the session's own time, so funes drains the spool newest session first.
	if info, err := os.Stat(sessionPath); err == nil {
		_ = os.Chtimes(tmp, info.ModTime(), info.ModTime())
	}
	if err := os.Rename(tmp, out); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return out, nil
}

// convertTree converts every session under a sessions tree into the spool: with a non-zero since,
// only those modified after it, and never except.
func convertTree(root, spool string, since time.Time, except string) []string {
	var written []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".jsonl") || p == except {
			return nil // an unreadable directory holds no session of PiG's
		}
		// One session that cannot be read costs its own capture, not everyone else's.
		if info, err := d.Info(); err == nil && (since.IsZero() || info.ModTime().After(since)) {
			if out, err := convert(p, spool); err == nil {
				written = append(written, out)
			}
		}
		return nil
	})
	return written
}
