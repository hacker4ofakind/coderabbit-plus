<p align="center">
  <img src="plugins/coderabbit-plus/assets/coderabbit-plus-logo.png" alt="CodeRabbit Plus logo" width="128">
</p>

<h1 align="center">CodeRabbit Plus</h1>

<p align="center">
  <a href="https://github.com/hacker4ofakind/coderabbit-plus/stargazers"><img src="https://img.shields.io/github/stars/hacker4ofakind/coderabbit-plus?style=for-the-badge&amp;logo=github&amp;label=Stars" alt="GitHub stars"></a>
  <a href="https://github.com/hacker4ofakind/coderabbit-plus/blob/main/LICENSE"><img src="https://img.shields.io/github/license/hacker4ofakind/coderabbit-plus?style=for-the-badge&amp;label=License" alt="MIT license"></a>
  <a href="https://go.dev/"><img src="https://img.shields.io/badge/Go-1.26%2B-00ADD8?style=for-the-badge&amp;logo=go&amp;logoColor=white" alt="Go 1.26 or newer"></a>
  <a href="https://developers.openai.com/plugins/build/plugins"><img src="https://img.shields.io/badge/Codex-Plugin-10A37F?style=for-the-badge" alt="Codex plugin"></a>
</p>

CodeRabbit Plus is an open-source Codex plugin and a small Go wrapper for deep, evidence-backed reviews of a GitHub pull request or latest remote commit. It produces a paste-ready handoff for the implementing agent, without modifying the repository being reviewed.

The plugin supplies the review workflow; the wrapper creates an isolated, detached worktree at the immutable revision, then invokes Codex against that copy. It does not check out, stage, commit, or otherwise change the active worktree.

## Install the plugin

The recommended installation is through the public marketplace in this repository. In Codex CLI, add the marketplace:

```text
codex plugin marketplace add hacker4ofakind/coderabbit-plus --sparse .agents/plugins
```

Restart the ChatGPT desktop app, open the Plugins Directory, select **CodeRabbit Plus**, and install it. The plugin gives Codex the `$coderabbit-plus` skill. It is installable from any Codex client that supports Git-backed plugin marketplaces.

For a standalone Codex CLI or IDE setup, clone this repository and copy `plugins/coderabbit-plus/skills/coderabbit-plus` to your user skill directory (`~/.agents/skills/coderabbit-plus` on macOS/Linux, or `%USERPROFILE%\.agents\skills\coderabbit-plus` on Windows). Restart Codex if it does not appear immediately.

## Install the wrapper

Requirements:

- Go 1.26 or newer
- Git
- GitHub CLI (`gh`), authenticated for the repository being reviewed
- Codex CLI, authenticated and available on `PATH`

After this repository is public, install the wrapper with:

```text
go install github.com/hacker4ofakind/coderabbit-plus@latest
```

The wrapper discovers the plugin-installed skill automatically. For a custom skill location, set `CODERABBIT_PLUS_SKILL` to the absolute path of its `SKILL.md`.

## Review commands

Run these inside a Git repository whose `origin` is a GitHub repository:

```text
coderabbit-plus pr 123
coderabbit-plus commit feature/my-branch --low
coderabbit-plus --max pr 123
```

Choose the review depth that fits the change:

- Default (no flag): **GPT-5.6 Sol** with **high** reasoning effort.
- `--low`: **GPT-5.6 Terra** with **medium** reasoning effort.
- `--max`: **GPT-6 Astra** with **max** reasoning effort.

`--low` and `--max` cannot be combined.

On success, only the final review is written to standard output. Progress, diagnostics, and Codex JSONL are kept off standard output so the result is easy to hand to an implementing agent.

## Develop locally

```text
go test ./...
go vet ./...
go build -o coderabbit-plus.exe .
```

The source and tests are dependency-free. The test suite fakes GitHub and Codex commands, so it does not spend model usage or contact the network.

## Repository layout

```text
main.go                          Go wrapper source
main_test.go                     Dependency-free tests
plugins/coderabbit-plus/         Portable Codex plugin
.agents/plugins/marketplace.json Git-backed marketplace entry
```

## Security model

The wrapper validates PR numbers, branch names, GitHub origins, and immutable commit IDs. It fetches review inputs into temporary refs, creates a detached disposable worktree, and verifies that Codex left that worktree clean before emitting its answer. Temporary refs and worktrees are cleaned up on success, failure, or cancellation.

Review findings and repository content are treated as untrusted data by the included skill. The skill is intentionally diff-scoped and excludes `docs/` and `.superpowers/` from review findings.

## License

[MIT](LICENSE)
