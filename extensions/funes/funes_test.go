package funes

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Run as `funes mcp`, the test binary is a fake funes MCP server.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_FUNES_MCP") == "1" {
		fakeServer()
		return
	}
	os.Exit(m.Run())
}

func fakeServer() {
	sc := bufio.NewScanner(os.Stdin)
	initialized := false
	for sc.Scan() {
		var req struct {
			ID     *int64         `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			continue
		}
		var result any
		switch req.Method {
		case "notifications/initialized":
			initialized = true
			continue
		case "initialize":
			result = map[string]any{"protocolVersion": protocolVersion}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "recall", "description": "d", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			args, _ := json.Marshal(req.Params["arguments"])
			result = map[string]any{"content": []any{
				map[string]any{"type": "text", "text": fmt.Sprintf("%v %s %v", req.Params["name"], args, initialized)},
				map[string]any{"type": "image"},
				map[string]any{"type": "text", "text": strings.Join(os.Args[1:], " ")},
			}}
		}
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		fmt.Printf("noise\n%s\n", out)
	}
}

func TestConvertMatchesThePiBundle(t *testing.T) {
	out, err := convert("testdata/session.jsonl", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	want, _ := os.ReadFile("testdata/expected.funes.jsonl")
	if string(got) != string(want) {
		t.Fatalf("turns file changed:\n got: %s\nwant: %s", got, want)
	}
	src, _ := os.Stat("testdata/session.jsonl")
	dst, _ := os.Stat(out)
	if !src.ModTime().Equal(dst.ModTime()) {
		t.Fatalf("turns file does not carry the session's time: %v vs %v", dst.ModTime(), src.ModTime())
	}
}

func TestConvertEdgeCases(t *testing.T) {
	dir := t.TempDir()
	session := filepath.Join(dir, "s.jsonl")
	lines := []string{
		`{"type":"session","cwd":"/x"}`,
		`{"type":"message","id":"a","timestamp":0,"message":{"role":"user","content":[{"type":"text","text":" \u3000 "}]}}`,
		`{"type":"message","id":"b","timestamp":1700000000123,"message":{"role":"user","content":[{"type":"text","text":"hi \ud83d\ude00"}]}}`,
		`{"type":"message","id":"c","message":{"role":"user","content":[{"type":"text","text":"lone \ud800"}]}}`,
		`{"type":"message","id":"d","parentId":"b","message":{"role":"assistant","content":[{"type":"toolCall","id":"t1","name":"bash","arguments":{"z":1, "a":"<b>"}}]}}`,
		`{"type":"message","id":"e","message":{"role":"toolResult","toolName":"bash","toolCallId":"t1","content":[{"type":"text","text":"out"},"raw",{"type":"image"}]}}`,
		`{"type":"message","id":"f","message":{"role":"system","content":"ignored"}}`,
		`{"type":"message","id":"g","message":{"role":"user","content":[{"type":"text","text":"cut`,
	}
	if err := os.WriteFile(session, []byte(strings.Join(lines, "\r\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	turns, err := turnsOf(session, "s")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tu := range turns {
		b, _ := json.Marshal(tu)
		got = append(got, string(b))
	}
	want := []string{
		`{"format":1,"session_id":"s","cwd":"/x","turn_uuid":"b","seq":0,"ts":"2023-11-14T22:13:20.123Z","role":"user","blocks":[{"block_type":"text","text":"hi 😀"}],"harness":"pig"}`,
		`{"format":1,"session_id":"s","cwd":"/x","turn_uuid":"d","parent_uuid":"b","seq":1,"ts":"","role":"assistant","blocks":[{"block_type":"tool_use","text":"{\"z\":1,\"a\":\"\u003cb\u003e\"}","tool_name":"bash","tool_use_id":"t1"}],"harness":"pig"}`,
		`{"format":1,"session_id":"s","cwd":"/x","turn_uuid":"e","seq":2,"ts":"","role":"tool","blocks":[{"block_type":"tool_result","text":"out\nraw","tool_name":"bash","tool_use_id":"t1"}],"harness":"pig"}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestLoneSurrogate(t *testing.T) {
	for line, want := range map[string]bool{
		`"\ud83d\ude00"`: false,
		`"\ud800"`:       true,
		`"\udc00"`:       true,
		`"\ud800x"`:      true,
		`"\\ud800"`:      false, // an escaped backslash, then text
		`"\u00e9\n"`:     false,
	} {
		if got := hasLoneSurrogate(line); got != want {
			t.Errorf("hasLoneSurrogate(%s) = %v, want %v", line, got, want)
		}
	}
}

func TestConvertTreeSince(t *testing.T) {
	root, spool := t.TempDir(), t.TempDir()
	old, fresh, skip := filepath.Join(root, "p", "old.jsonl"), filepath.Join(root, "p", "new.jsonl"), filepath.Join(root, "q", "skip.jsonl")
	for _, p := range []string{old, fresh, skip} {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"x"}]}}`), 0o644)
	}
	since := time.Now().Add(-time.Minute)
	os.Chtimes(old, since.Add(-time.Hour), since.Add(-time.Hour))
	written := convertTree(root, spool, since, skip)
	if len(written) != 1 || filepath.Base(written[0]) != "new.funes.jsonl" {
		t.Fatalf("written = %v", written)
	}
	if all := convertTree(root, spool, time.Time{}, ""); len(all) != 3 {
		t.Fatalf("a first sweep converts everything, got %v", all)
	}
}

func TestMCPBridge(t *testing.T) {
	self, _ := os.Executable()
	m := newFunesMCP(self, []string{"mcp", "org/mem"}, append(os.Environ(), "FAKE_FUNES_MCP=1"))
	defer m.close()
	tools, err := m.listTools(context.Background())
	if err != nil || len(tools) != 1 || tools[0].Name != "recall" {
		t.Fatalf("tools = %v, %v", tools, err)
	}
	text, err := m.callTool(context.Background(), "recall", map[string]any{"query": "q"})
	if want := "recall {\"query\":\"q\"} true\nmcp org/mem"; err != nil || text != want {
		t.Fatalf("callTool = %q, %v; want %q", text, err, want)
	}
	// A server that went away is respawned on the next call.
	m.close()
	if _, err := m.callTool(context.Background(), "recall", nil); err != nil {
		t.Fatalf("after close: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.callTool(ctx, "recall", nil); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("cancelled call: %v", err)
	}
}

func TestMCPStartFailure(t *testing.T) {
	m := newFunesMCP(filepath.Join(t.TempDir(), "no-funes"), []string{"mcp"}, os.Environ())
	if _, err := m.listTools(context.Background()); err == nil || !strings.Contains(err.Error(), "failed to start") {
		t.Fatalf("err = %v", err)
	}
}
