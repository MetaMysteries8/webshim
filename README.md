# webshim

A WebSim agent that runs entirely in your terminal.

Talk to an LLM, and it inspects and edits your WebSim projects through the
documented revision lifecycle — branch a draft, write, finalize, verify,
promote, verify again — with every live-state change gated behind a permission
prompt you control.

Built in Go on the [Charm](https://charm.sh) stack: Bubble Tea, Bubbles, Lip
Gloss, Huh, Glamour, Glow, Log, Harmonica, Wish, and
[Fantasy](https://charm.land/fantasy) for the agent loop.

> **Status: in development.** The WebSim API client and its test suite are done.
> The catalog, agent, and TUI layers are being built on top.

## Why

WebSim's web-app API is internal and unversioned. Editing a published project
correctly means holding a specific sequence — and every step has a way to go
wrong that silently costs you the live site. `webshim` encodes that sequence
once, in one place, and refuses to skip a step:

- Published revisions are immutable; every edit branches a fresh draft.
- A draft is never made current. It is finalized first, and the finalization is
  verified before promotion is even attempted.
- Before promoting, the project is re-read. If another actor moved
  `current_version` while your edit was in flight, the flow stops and tells you
  to rebase rather than silently discarding their work.
- Nothing reports success until `GET /projects/{id}` confirms the new version.
- Bearer tokens are a distinct type whose `String`, `GoString`, and
  `MarshalJSON` all redact. They cannot land in a log line by accident.

The rules come from `docs/WEBSIM_API_AGENT_PLAYBOOK.txt`. Where the playbook and
the code disagree, the code is wrong.

## Permission modes

Every tool carries a risk class, and the mode decides what needs your approval.

| | Read | Edit a draft | Change live state |
|---|:--:|:--:|:--:|
| **Manual** | ask | ask | ask |
| **Normal** (default) | auto | auto | **ask** |
| **YOLO** | auto | auto | auto |

"Change live state" means finalize, promote, rollback, delete, project
creation, metadata changes, comments, and shell commands.

## Install

Requires Go 1.26 or newer.

```sh
go install github.com/MetaMysteries8/webshim@latest
```

## Setup

Authentication resolves in this order, first hit wins:

1. a per-project `bearer` in `projects.config.json`
2. `WEBSIM_BEARER`, then `bearer`, then `WEBSIM_TOKEN`
3. `authToken` from the file named by `WEBSIM_CLI_CONFIG`
4. `authToken` from `~/.websim-cli.json`

The last one is what [`websim-cli`](https://www.npmjs.com/package/websim-cli)
writes, so the simplest setup is:

```sh
websim-cli login
```

For the model provider, [Charm Hyper](https://hyper.charm.land) is the default:

```sh
export HYPER_API_KEY="sk-hyper-..."
```

Any provider in the [models.dev](https://models.dev) catalog can be selected
instead.

Then check everything resolved:

```sh
webshim doctor
```

## Usage

```sh
webshim                       # the TUI
webshim ls <alias>            # inspect a project
webshim publish <alias> <dir> # publish a directory
webshim rollback <alias> <v>  # make an earlier revision current
webshim doctor                # verify credentials and connectivity
```

The non-TUI commands print the playbook's output contract as JSON, so they
script cleanly:

```json
{"ok":true,"action":"publish_edit","project":"demo","previous_version":11,
 "current_version":12,"changed_paths":["index.html"],"verified":true}
```

Add `--dry-run` to any mutating command to see where a flow would write without
sending anything.

## Development

```sh
go build ./...
go vet ./...
go test ./...
go test -race ./...   # needs cgo and a C compiler
```

`internal/websim` is the client and depends only on the standard library — no
UI, no LLM. It is tested against an in-process fake that speaks the documented
response shapes, including the multipart asset format and the failure modes the
playbook warns about (a finalize that doesn't take, a promotion whose response
is lost, a competing actor mid-flow).

## Safety

`projects.config.json` can hold bearer tokens and is gitignored. Do not commit
it; `projects.config.example.json` is the template. Never embed a token in a
WebSim page — anything you publish is public client-side code.

## License

MIT
