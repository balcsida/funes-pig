# funes-pig

A [funes](https://github.com/huggingface/funes) integration for [PiG](https://github.com/MichaelKinsy/PiG),
written as a native Go extension on PiG's extension SDK. It gives PiG the funes read tools and keeps
the memory current as you work.

It is a Go port of the [`pi` bundle](https://github.com/huggingface/funes-integrations/tree/main/pi)
Hugging Face maintains, and behaves the same way:

- **Tools.** The extension spawns `funes mcp` once over stdio, keeps it warm for the session, and
  registers whatever `tools/list` returns (`recall`, `get`, …), so PiG sees exactly the surface of
  the funes binary on `PATH`.
- **Per turn.** On `turn_end` it converts the session PiG is writing into funes's spool
  (`~/.funes/spool/pig/<session>.funes.jsonl`, [the turns format](https://github.com/huggingface/funes/blob/main/docs/funes-jsonl.md)),
  converts any other session changed since the last turn, and runs `funes index --harness pig`
  detached.
- **Session boundaries.** With a memory bound, `session_shutdown` (other than a reload) and a fresh
  `session_start` run `funes push` after indexing.
- **History.** The first start after `funes add pig` converts every session PiG has already
  written. PiG's sessions use pi's format, so the mapping is pi's, with `harness` set to `pig`.

The directory is both a funes bundle (`manifest.json`, `setup`) and a PiG Package (`package.json`,
`extensions/funes`).

## Install

With `funes` and `pig` on your `PATH`:

```sh
git clone https://github.com/balcsida/funes-pig
funes add pig --from ./funes-pig            # or: funes add pig <memory> --from ./funes-pig
```

funes copies the bundle to `~/.funes/agents/pig` and runs `setup add`, which records the spool and
the bound memory there and runs `pig install ~/.funes/agents/pig`. Restart PiG; its next start
converts your history.

To try the tools without installing, load the extension for one run: `pig -e ./extensions/funes`.
Without an install it has no spool to write to, so it indexes nothing.

## Remove

```sh
funes remove pig
```

Unregisters the Package, drops the spool and the extracted files. Your memory and PiG's sessions are
untouched.

## Configuration

- `FUNES_BIN`: the funes binary (default `funes` from `PATH`).
- `FUNES_MEMORY`: the memory to recall from and publish to, overriding the one bound at install.
- `FUNES_HOME`: funes's home (default `~/.funes`); the extension finds its install under
  `$FUNES_HOME/agents/pig`.
- `PIG_CODING_AGENT_SESSION_DIR` / `PIG_CODING_AGENT_DIR`: where PiG keeps sessions, honored the
  way PiG honors them.

The automation logs to `~/.funes/agents/pig/scripts/funes-sync.log`.

## Requirements

- PiG 0.5.3 or newer, and the Go toolchain PiG builds Go extensions with (`pig setup go`).
- `funes` on `PATH`, and a memory it can read: local, or an `hf://` remote.

## Development

```sh
cd extensions/funes && go test -race ./...
```

The converter test checks the output byte for byte against the pi bundle's expected turns file, and
the MCP bridge is tested against a fake `funes mcp` (the test binary itself).

## License

Apache-2.0. The conversion mapping and the automation script come from
[huggingface/funes-integrations](https://github.com/huggingface/funes-integrations).
