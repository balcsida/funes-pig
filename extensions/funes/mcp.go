package funes

// A minimal MCP stdio client for one `funes mcp` child, kept warm for the session so the embedder
// and reranker stay loaded across calls. stdout is the JSON-RPC channel (one message per line),
// stderr is logs.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	protocolVersion  = "2024-11-05" // matches funes's rmcp server
	callTimeout      = 120 * time.Second
	handshakeTimeout = 10 * time.Second // PiG's startup waits on these, so they don't get a recall's bound
)

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type rpcResponse struct {
	ID     *int64          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type funesMCP struct {
	bin  string
	args []string
	env  []string

	startMu sync.Mutex // serializes spawn and handshake
	mu      sync.Mutex
	ready   bool
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	nextID  int64
	pending map[int64]chan rpcResponse
}

func newFunesMCP(bin string, args, env []string) *funesMCP {
	return &funesMCP{bin: bin, args: args, env: env}
}

// ensureStarted spawns and initializes the server unless one is ready. startMu keeps a second
// caller from racing a second child, or sending ahead of the handshake.
func (m *funesMCP) ensureStarted(ctx context.Context) error {
	m.startMu.Lock()
	defer m.startMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ready {
		return nil
	}
	cmd := exec.Command(m.bin, m.args...)
	cmd.Env = m.env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = io.Discard // drained, so the pipe never blocks
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("funes mcp failed to start: %w", err)
	}
	m.cmd, m.stdin, m.pending = cmd, stdin, map[int64]chan rpcResponse{}
	go m.read(cmd, stdout)

	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	init := map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "pig-funes-bridge", "version": "0.1.0"},
	}
	if _, err := m.requestLocked(hctx, "initialize", init); err != nil {
		// Tear a half-started child down, so the next call respawns instead of failing forever.
		m.stopLocked(err)
		return err
	}
	if err := m.writeLocked(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized", "params": map[string]any{}}); err != nil {
		m.stopLocked(err)
		return err
	}
	m.ready = true
	return nil
}

// read delivers each response to its caller until the child goes away, then fails what is pending.
func (m *funesMCP) read(cmd *exec.Cmd, stdout io.Reader) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		var resp rpcResponse
		if line == "" || json.Unmarshal([]byte(line), &resp) != nil || resp.ID == nil {
			continue // stray output, or a notification
		}
		m.mu.Lock()
		ch := m.pending[*resp.ID]
		delete(m.pending, *resp.ID)
		m.mu.Unlock()
		if ch != nil {
			ch <- resp
		}
	}
	err := cmd.Wait()
	m.mu.Lock()
	if m.cmd == cmd {
		m.stopLocked(fmt.Errorf("funes mcp exited (%v)", err))
	}
	m.mu.Unlock()
}

func (m *funesMCP) stopLocked(err error) {
	if m.cmd == nil {
		return
	}
	_ = m.cmd.Process.Kill()
	for id, ch := range m.pending {
		msg := err.Error()
		ch <- rpcResponse{Error: &struct {
			Message string `json:"message"`
		}{msg}}
		delete(m.pending, id)
	}
	m.cmd, m.stdin, m.ready = nil, nil, false
}

func (m *funesMCP) writeLocked(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = m.stdin.Write(append(data, '\n'))
	return err
}

// requestLocked sends with mu held and waits with it released.
func (m *funesMCP) requestLocked(ctx context.Context, method string, params any) (json.RawMessage, error) {
	m.nextID++
	id := m.nextID
	ch := make(chan rpcResponse, 1)
	m.pending[id] = ch
	if err := m.writeLocked(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		delete(m.pending, id)
		return nil, fmt.Errorf("funes mcp went away: %w", err)
	}
	m.mu.Unlock()
	defer m.mu.Lock()
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, errors.New(resp.Error.Message)
		}
		return resp.Result, nil
	case <-ctx.Done():
		m.mu.Lock()
		delete(m.pending, id)
		m.mu.Unlock()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("funes %s timed out", method)
		}
		return nil, fmt.Errorf("funes %s cancelled", method)
	}
}

func (m *funesMCP) request(ctx context.Context, method string, params any, timeout time.Duration) (json.RawMessage, error) {
	if err := m.ensureStarted(ctx); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.ready {
		return nil, errors.New("funes mcp not running")
	}
	return m.requestLocked(ctx, method, params)
}

// listTools is the tools this funes binary exposes, as MCP declares them.
func (m *funesMCP) listTools(ctx context.Context) ([]mcpTool, error) {
	raw, err := m.request(ctx, "tools/list", map[string]any{}, handshakeTimeout)
	if err != nil {
		return nil, err
	}
	var result struct {
		Tools []mcpTool `json:"tools"`
	}
	return result.Tools, json.Unmarshal(raw, &result)
}

// callTool calls an MCP tool and flattens its text content to a string.
func (m *funesMCP) callTool(ctx context.Context, name string, args map[string]any) (string, error) {
	raw, err := m.request(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, callTimeout)
	if err != nil {
		return "", err
	}
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", err
	}
	var texts []string
	for _, c := range result.Content {
		if c.Type == "text" {
			texts = append(texts, c.Text)
		}
	}
	return strings.Join(texts, "\n"), nil
}

func (m *funesMCP) close() {
	m.mu.Lock()
	m.stopLocked(errors.New("funes mcp closed"))
	m.mu.Unlock()
}
