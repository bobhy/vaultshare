# Obsidian CLI cheat sheet (via `obsidian-testvault`)

Source: <https://obsidian.md/help/cli> and `obsidian-cli --help` (Obsidian 1.15). Requires the
Obsidian 1.12+ installer and **Settings → General → Command line interface** enabled.

Form: `obsidian-testvault <vault-folder> <command> [param=value ...] [flag ...]`.
Quote values with spaces: `code="app.vault.getName()"`. Use `\n` / `\t` inside values.

## Plugin lifecycle

| Goal | Command |
| :--- | :--- |
| Plugin info / installed? | `plugin id=<id>` |
| Enabled community plugins | `plugins:enabled filter=community` |
| Enable / disable | `plugin:enable id=<id> filter=community` / `plugin:disable id=<id> filter=community` |
| Reload after copying a new build | `plugin:reload id=<id>` |
| Reload the whole window | `reload` (vault) / `restart` (app) |

## Commands and navigation

| Goal | Command |
| :--- | :--- |
| List plugin commands | `commands filter=<id>` |
| Run a command | `command id=<id>:<command-id>` |
| Open a note (needed for editor-only commands) | `open path=<folder/note.md>` or `open file=<name>` |
| Create a scratch note | `create path=<path> content="..." open` |
| Open tabs / layout | `tabs`, `workspace` |

## Inspection

| Goal | Command |
| :--- | :--- |
| Uncaught JS errors | `dev:errors` (clear with `dev:errors clear`) |
| Console messages | `dev:debug on` once, then `dev:console level=error limit=20` |
| Screenshot | `dev:screenshot path=<absolute .png path>` (prints the path) |
| DOM query | `dev:dom selector=<css> [text] [inner] [all] [total] [attr=<name>] [css=<prop>]` |
| Computed CSS with source | `dev:css selector=<css> prop=<name>` |
| Arbitrary JS (has `app`) | `eval code="<js>"`; prints the last expression's value; top-level `await` allowed |
| Open settings | `command id=app:open-settings` (opens a separate window; see Quirks) |
| Mobile layout | `dev:mobile on` / `dev:mobile off` |
| Raw DevTools Protocol | `dev:cdp method=<Domain.method> params=<json>` |

## Quirks

- **Exit status is always 0**, even for "Vault not found." or command errors: judge by output text.
- `dev:console` needs `dev:debug on` first ("Debugger not attached" otherwise). The debugger
  stays attached until `dev:debug off` or app restart.
- Editor-scoped commands (`editorCallback`) are hidden from `commands` and fail to run unless a
  markdown note is the active view; `open` one first.
- `eval` output is prefixed with `=>`. Despite the CLI help text, a top-level `return` is a
  syntax error ("Illegal return statement"); the last expression's value is printed. Returning an
  Obsidian object (e.g. `app.setting.openTabById(...)`) fails with "Converting circular structure
  to JSON", so end with a string or `JSON.stringify(...)` of plain data.
- Obsidian 1.15 opens **Settings in a separate window**. `dev:screenshot`, `dev:dom`, `dev:css`
  and `eval` DOM queries see only the main vault window, so the settings UI can't be inspected
  through the CLI (verified 2026-11-20 on 1.15.1).
- If Obsidian isn't running, the first CLI call launches it.
