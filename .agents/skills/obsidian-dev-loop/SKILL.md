---
name: obsidian-dev-loop
description: >-
  Build, deploy, reload and interactively test this Obsidian plugin inside a live Obsidian
  test vault (~/obsidian-test/<vault>) via the official Obsidian CLI: run plugin commands, take
  and inspect screenshots, query the DOM, read JS errors and console output, and eval against
  `app`. Use when asked to run, try, smoke-test, screenshot or debug the plugin in Obsidian, or
  to confirm a UI/behavior change works in the real app rather than only in unit tests.
---

# Obsidian plugin dev loop

Drive a **running** Obsidian instance against a **test vault** to verify plugin changes.

## Safety rules

- Only use test vaults: folders directly under `~/obsidian-test/`. Every CLI call goes through
  `obsidian-testvault <vault-folder> ...`, which resolves the vault by ID and refuses anything
  else. Never call `obsidian-cli` / `obsidian` directly and never pass `vault=`.
- Default vault: `test1`, unless the user names another or more than one is needed.
- Don't read or edit `<vault>/.obsidian/plugins/<id>/data.json`. It may hold credentials or
  tokens. Change settings through the plugin's UI or commands.
- Ask before running commands that have external side effects (for VaultShare, anything that
  syncs with Google Drive: `run-full-scan`, `flush-pending-sync`, `resume-sync`), unless the
  user already asked for that in this task.
- A missing vault means it hasn't been registered: ask the user to open the folder once in
  Obsidian (**Open another vault → Open folder as vault**). Don't edit `obsidian.json`.

## Prerequisites (check once per session)

```sh
command -v obsidian-testvault          # wrapper in ~/.local/bin
obsidian-testvault test1 vault         # prints name/path; launches Obsidian if needed
```

If the wrapper is missing, stop and tell the user. Don't work around it.

## Loop

1. **Build and deploy.** From the repo root:
   ```sh
   .agents/skills/obsidian-dev-loop/scripts/deploy.sh test1 --build
   ```
   This runs `npm run build`, copies `main.js`, `manifest.json` and `styles.css` into the vault's
   plugin folder (leaving `data.json` alone), enables the plugin if needed, reloads it, and prints
   `dev:errors`. It exits non-zero if the plugin didn't load. Leave off `--build` if
   `npm run dev` is already rebuilding `main.js` in watch mode.
2. **Exercise the change.** List commands with `obsidian-testvault test1 commands filter=vaultshare`
   and run one with `obsidian-testvault test1 command id=vaultshare:<command-id>`. Editor-only
   commands (`find-next-conflict`, `find-previous-conflict`) need an open note first:
   `obsidian-testvault test1 open path=<note.md>`.
   **Settings tab limitation:** since Obsidian 1.15, Settings opens in its own window with its
   own document. `dev:screenshot`, `dev:dom` and `eval` only see the main window, so the CLI can't
   inspect the plugin's settings UI. To check settings visually, ask the user to look, or offer a
   niri window screenshot (`niri msg -j windows` to find the "Settings - <vault>" window id, then
   `niri msg action screenshot-window --id <id>`; this saves to niri's `screenshot-path` and
   **replaces the user's clipboard**, so ask first).
3. **Observe.**
   - Screenshot: `obsidian-testvault test1 dev:screenshot path=<scratch dir>/<name>.png`, then
     view the PNG with the Read tool / image viewer. Prefer the agent's scratch or temp
     directory over the repo.
   - Errors: `obsidian-testvault test1 dev:errors`.
   - Console: `dev:debug on` once, then `dev:console level=error limit=20` (or `level=warn`).
   - DOM/state: `dev:dom selector=<css> text` or `eval code="<js using app>"`. `eval` prints
     the value of the last expression (`return` is a syntax error; top-level `await` works).
     End with a string or `JSON.stringify(...)`: returning an Obsidian object fails with
     "Converting circular structure to JSON".
4. **Compare expected vs. seen.** State what the change should have produced, then what the
   screenshot, DOM or errors show. Fix the code and repeat from step 1.
5. **Clean up.** `dev:debug off` if you attached it, `dev:mobile off` if you enabled it, and
   close any modals or settings you opened (`eval code="app.setting.close(); 'closed'"`).

Also check mobile layout (`isDesktopOnly` is false): `dev:mobile on`, screenshot, `dev:mobile off`.

## Reporting

Report which vault was used, the commands run, the screenshots taken (with paths), and any
errors captured. Screenshots and live checks don't replace the project's required checks
(`npm run test:go && npm test && npm run build && npm run lint`).

## Reference

- Full command syntax, parameters and CLI quirks (always exits 0, debugger needed for console,
  hidden editor commands): [references/cli-cheatsheet.md](references/cli-cheatsheet.md)
- Official docs: <https://obsidian.md/help/cli>
