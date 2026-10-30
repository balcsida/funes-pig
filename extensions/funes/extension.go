// Package funes is a PiG extension that gives PiG the funes read tools and keeps the memory current
// as you work. A Go port of the pi bundle in huggingface/funes-integrations.
//
// The tools aren't written out here: the extension spawns `funes mcp` once over stdio and registers
// whatever `tools/list` returns, passing each MCP schema through as the tool's parameters.
//
// `funes` is taken from PATH; FUNES_BIN overrides it. The memory recalled from (and published to) is
// FUNES_MEMORY, else the one `funes add pig <memory>` bound in a `memory` file in the bundle, else
// the local memory.
package funes

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// bundleDir is where `funes add pig` installed this bundle, and keeps its state: funes's contract
// fixes it at `$FUNES_HOME/agents/<id>`. A compiled extension cannot find its own source the way a
// script can, and a run from a checkout simply finds no state there and automates nothing.
func bundleDir() string {
	home := os.Getenv("FUNES_HOME")
	if home == "" {
		userHome, _ := os.UserHomeDir()
		home = filepath.Join(userHome, ".funes")
	}
	return filepath.Join(home, "agents", "pig")
}

// bound is a file `setup add` wrote into the bundle, or "" when there is none.
func bound(dir, name string) string {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// funesEnv names funes and PiG in funes's Hub requests: hf-hub appends HF_HUB_USER_AGENT_ORIGIN to
// its User-Agent. An origin already set is kept in front.
func funesEnv() []string {
	origin := "funes; agent/pig"
	if prev := os.Getenv("HF_HUB_USER_AGENT_ORIGIN"); prev != "" {
		origin = prev + "; " + origin
	}
	return append(os.Environ(), "HF_HUB_USER_AGENT_ORIGIN="+origin)
}

// sessionsRoot is where PiG keeps its sessions, one directory per project under it, with the
// overrides PiG honors.
func sessionsRoot() string {
	if dir := os.Getenv("PIG_CODING_AGENT_SESSION_DIR"); dir != "" {
		return dir
	}
	agent := os.Getenv("PIG_CODING_AGENT_DIR")
	if agent == "" {
		home, _ := os.UserHomeDir()
		agent = filepath.Join(home, ".pig", "agent")
	}
	return filepath.Join(agent, "sessions")
}

type automation struct {
	dir    string // the bundle
	spool  string // the directory funes drains for this agent, recorded by `setup add`
	memory string
}

// run starts funes-index.sh detached and forgets it, as the pi bundle does: no failure of its is
// worth disturbing the session with, and its own log says what happened.
func (a automation) run(args ...string) {
	script := filepath.Join(a.dir, "scripts", "funes-index.sh")
	if a.spool == "" {
		return
	}
	if _, err := os.Stat(script); err != nil {
		return
	}
	cmd := exec.Command("sh", append([]string{script}, args...)...)
	cmd.Env = funesEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if cmd.Start() == nil {
		go cmd.Wait() // reaps the child; the script reports to its own log, not to us
	}
}

func (a automation) publish() {
	if a.memory != "" {
		a.run("--publish", a.memory)
	}
}

// sweep converts the sessions changed since the last sweep: those a PiG that died mid-turn never
// converted. Before the first sweep there is no mark, and the whole history is converted: that is the
// seed. except is the session in progress, converted already. It reports whether it wrote anything.
func (a automation) sweep(root, except string) bool {
	if a.spool == "" {
		return false
	}
	mark := filepath.Join(a.dir, "swept")
	var since time.Time
	if info, err := os.Stat(mark); err == nil {
		since = info.ModTime()
	}
	// Stamped before the sweep, so a session written while it runs is swept again next turn.
	now := time.Now()
	written := convertTree(root, a.spool, since, except)
	if os.WriteFile(mark, nil, 0o644) == nil {
		_ = os.Chtimes(mark, now, now)
	}
	return len(written) > 0
}

// sweepRoot is the sessions root when the session in progress is under it; else the session's own
// directory, since PiG writes straight into a `--session-dir` and a walk any higher would take in
// files that are not sessions.
func sweepRoot(file string) string {
	root := sessionsRoot()
	if file == "" || strings.HasPrefix(file, root+string(filepath.Separator)) {
		return root
	}
	return filepath.Dir(file)
}

// sessionFile is the session PiG is writing, or "" for an ephemeral one.
func sessionFile(ctx sdk.Context) string {
	file, err := ctx.GetSessionFile()
	if err != nil || file == nil {
		return ""
	}
	return *file
}

// Extension is the funes extension.
func Extension() *sdk.Extension {
	ext := sdk.New("funes")
	dir := bundleDir()
	auto := automation{dir: dir, spool: bound(dir, "spool"), memory: bound(dir, "memory")}
	if m := strings.TrimSpace(os.Getenv("FUNES_MEMORY")); m != "" {
		auto.memory = m
	}

	bin := os.Getenv("FUNES_BIN")
	if bin == "" {
		bin = "funes"
	}
	args := []string{"mcp"}
	if auto.memory != "" {
		args = append(args, auto.memory)
	}
	funes := newFunesMCP(bin, args, funesEnv())

	failure := ""
	tools, err := funes.listTools(context.Background())
	if err != nil {
		failure = "funes recall is unavailable: " + err.Error()
	}
	for _, tool := range tools {
		name := tool.Name
		ext.RegisterTool(sdk.ToolDefinition{
			Name:        name,
			Label:       "funes " + name,
			Description: tool.Description,
			Parameters:  tool.InputSchema,
			Execute: func(ctx sdk.Context, params map[string]any) (any, error) {
				signal := ctx.Signal()
				if signal == nil {
					signal = context.Background()
				}
				text, err := funes.callTool(signal, name, params)
				if err != nil {
					text = name + " error: " + err.Error()
				}
				return map[string]any{"content": text}, nil
			},
		})
	}

	ext.OnEvent(sdk.EventSessionStart, func(ctx sdk.Context, data map[string]any) (any, error) {
		if failure != "" {
			ctx.Notify(failure, "warning") // at load it would land inside PiG's own startup
		}
		if reason, _ := data["reason"].(string); reason != "startup" {
			return nil, nil
		}
		// A fresh process is the one start with no shutdown behind it: it catches up whatever a
		// process that never shut down cleanly left unconverted or unpublished.
		seeded := auto.sweep(sweepRoot(sessionFile(ctx)), "")
		if auto.memory != "" {
			auto.publish()
		} else if seeded {
			auto.run()
		}
		return nil, nil
	})

	// Per turn, so a session killed mid-flight is indexed up to its last completed turn.
	ext.OnEvent(sdk.EventTurnEnd, func(ctx sdk.Context, _ map[string]any) (any, error) {
		if auto.spool == "" {
			return nil, nil
		}
		file := sessionFile(ctx)
		if file != "" {
			_, _ = convert(file, auto.spool)
		}
		auto.sweep(sweepRoot(file), file)
		auto.run()
		return nil, nil
	})

	// A reload replaces the extension instance without ending the session: nothing to publish.
	ext.OnEvent(sdk.EventSessionShutdown, func(_ sdk.Context, data map[string]any) (any, error) {
		if reason, _ := data["reason"].(string); reason != "reload" {
			auto.publish()
		}
		funes.close()
		return nil, nil
	})
	return ext
}
