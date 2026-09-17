package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const version = "1.0.0"

const (
	skillEnvVar  = "CODERABBIT_PLUS_SKILL"
	skillFile    = "SKILL.md"
	skillDirName = "coderabbit-plus"
)

const (
	exitUsage    = 2
	exitSetup    = 3
	exitCodex    = 4
	exitCanceled = 5
)

type cliError struct {
	code    int
	message string
}

func (e *cliError) Error() string { return e.message }

type request struct {
	mode, value, model, effort string
	low, max                   bool
}

func parseArgs(args []string) (request, error) {
	r := request{model: "gpt-5.6-sol", effort: "high"}
	var positional []string
	for _, arg := range args {
		switch arg {
		case "--low":
			r.low = true
		case "--max":
			r.max = true
		case "--help", "-h":
			return r, &cliError{exitUsage, "help"}
		case "--version":
			return r, &cliError{exitUsage, "version"}
		default:
			if strings.HasPrefix(arg, "-") {
				return r, &cliError{exitUsage, "unknown flag: " + arg}
			}
			positional = append(positional, arg)
		}
	}
	if r.low && r.max {
		return r, &cliError{exitUsage, "--low and --max cannot be used together"}
	}
	if len(positional) != 2 || (positional[0] != "pr" && positional[0] != "commit") {
		return r, &cliError{exitUsage, "usage: coderabbit-plus [--low|--max] <pr <number>|commit <branch>>"}
	}
	r.mode, r.value = positional[0], positional[1]
	if r.mode == "pr" && !validPR(r.value) {
		return r, &cliError{exitUsage, "PR number must be a positive integer"}
	}
	if r.mode == "commit" && !validBranch(r.value) {
		return r, &cliError{exitUsage, "invalid branch name"}
	}
	if r.low {
		r.model, r.effort = "gpt-5.6-terra", "medium"
	} else if r.max {
		r.model, r.effort = "gpt-6-astra", "max"
	}
	return r, nil
}

func validPR(s string) bool { return regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(s) }
func validBranch(s string) bool {
	if s == "" || len(s) > 240 || strings.HasPrefix(s, "-") || strings.ContainsAny(s, " ~^:?*[\\[") || strings.Contains(s, "..") || strings.Contains(s, "@{") || strings.HasSuffix(s, ".") || strings.HasSuffix(s, ".lock") || strings.Contains(s, "//") {
		return false
	}
	return regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`).MatchString(s)
}

type runner struct {
	root     string
	lookPath func(string) (string, error)
	command  func(context.Context, string, ...string) *exec.Cmd
}

func newRunner() runner { return runner{lookPath: exec.LookPath, command: exec.CommandContext} }

func (r runner) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := r.command(ctx, name, args...)
	cmd.Dir = r.root
	cmd.Stdin = strings.NewReader("")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, &cliError{exitCanceled, "review canceled"}
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(msg)
	}
	return out, nil
}

func (r runner) mustTool(name string) error {
	if _, err := r.lookPath(name); err != nil {
		return &cliError{exitSetup, name + " is required but was not found on PATH"}
	}
	return nil
}

func (r *runner) preflight(ctx context.Context) error {
	for _, tool := range []string{"git", "gh", "codex"} {
		if err := r.mustTool(tool); err != nil {
			return err
		}
	}
	out, err := r.run(ctx, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return &cliError{exitSetup, "not inside a Git repository"}
	}
	r.root = strings.TrimSpace(string(out))
	remote, err := r.run(ctx, "git", "remote", "get-url", "origin")
	if err != nil || !isGitHubRemote(strings.TrimSpace(string(remote))) {
		return &cliError{exitSetup, "origin must be a GitHub repository"}
	}
	return nil
}

func isGitHubRemote(remote string) bool {
	return strings.Contains(strings.ToLower(remote), "github.com:") || strings.Contains(strings.ToLower(remote), "github.com/")
}

type prInfo struct {
	BaseRefName string `json:"baseRefName"`
	BaseRefOid  string `json:"baseRefOid"`
	HeadRefOid  string `json:"headRefOid"`
}
type revision struct{ base, head, clean string }

func (r runner) resolvePR(ctx context.Context, number string, refs tempRefs) (revision, error) {
	for attempt := 0; attempt < 2; attempt++ {
		out, err := r.run(ctx, "gh", "pr", "view", number, "--json", "baseRefName,baseRefOid,headRefOid")
		if err != nil {
			return revision{}, &cliError{exitSetup, "could not read GitHub PR " + number}
		}
		var pr prInfo
		if json.Unmarshal(out, &pr) != nil || !validSHA(pr.BaseRefOid) || !validSHA(pr.HeadRefOid) || !validBranch(pr.BaseRefName) {
			return revision{}, &cliError{exitSetup, "GitHub returned incomplete PR revision data"}
		}
		if _, err = r.run(ctx, "git", "fetch", "--no-tags", "origin", "+refs/heads/"+pr.BaseRefName+":"+refs.base, "+refs/pull/"+number+"/head:"+refs.head); err != nil {
			return revision{}, &cliError{exitSetup, "could not fetch immutable PR revisions"}
		}
		base, e1 := r.revParse(ctx, refs.base)
		head, e2 := r.revParse(ctx, refs.head)
		if e1 == nil && e2 == nil && base == pr.BaseRefOid && head == pr.HeadRefOid {
			mergeOut, mergeErr := r.run(ctx, "git", "merge-base", refs.base, refs.head)
			if mergeErr != nil || !validSHA(strings.TrimSpace(string(mergeOut))) {
				return revision{}, &cliError{exitSetup, "could not determine the PR merge base"}
			}
			return revision{base: strings.TrimSpace(string(mergeOut)), head: head, clean: "No issues found in PR #" + number + "."}, nil
		}
		if attempt == 1 {
			return revision{}, &cliError{exitSetup, "PR moved during setup; retry the review"}
		}
	}
	panic("unreachable")
}

func (r runner) resolveCommit(ctx context.Context, branch string, refs tempRefs) (revision, error) {
	out, err := r.run(ctx, "git", "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	if err != nil {
		return revision{}, &cliError{exitSetup, "could not resolve origin/" + branch}
	}
	fields := strings.Fields(string(out))
	if len(fields) < 1 || !validSHA(fields[0]) {
		return revision{}, &cliError{exitSetup, "origin branch was not found"}
	}
	head := fields[0]
	if _, err = r.run(ctx, "git", "fetch", "--no-tags", "origin", "+refs/heads/"+branch+":"+refs.head); err != nil {
		return revision{}, &cliError{exitSetup, "could not fetch origin/" + branch}
	}
	fetched, err := r.revParse(ctx, refs.head)
	if err != nil || fetched != head {
		return revision{}, &cliError{exitSetup, "branch moved during setup; retry the review"}
	}
	base, err := r.revParse(ctx, head+"^")
	if err != nil {
		return revision{}, &cliError{exitSetup, "latest commit has no parent"}
	}
	clean := "No issues found in commit " + head[:12] + "."
	// PR discovery only affects wording and must never prevent a commit review.
	if pr, err := r.run(ctx, "gh", "pr", "list", "--head", branch, "--state", "open", "--json", "number", "--limit", "1"); err == nil {
		var rows []struct {
			Number int `json:"number"`
		}
		if json.Unmarshal(pr, &rows) == nil && len(rows) == 1 && rows[0].Number > 0 {
			clean = fmt.Sprintf("No issues found in PR #%d.", rows[0].Number)
		}
	}
	return revision{base: base, head: head, clean: clean}, nil
}

func validSHA(s string) bool { return regexp.MustCompile(`^[0-9a-fA-F]{40}$`).MatchString(s) }
func (r runner) revParse(ctx context.Context, ref string) (string, error) {
	out, err := r.run(ctx, "git", "rev-parse", ref+"^{commit}")
	return strings.TrimSpace(string(out)), err
}

// installedSkillPath finds the public plugin's installed skill without tying the
// wrapper to a particular username or client cache version. An explicit path is
// useful for custom installations and takes precedence over the standard paths.
func installedSkillPath() (string, error) {
	if configured := os.Getenv(skillEnvVar); configured != "" {
		if isRegularFile(configured) {
			return configured, nil
		}
		return "", &cliError{exitSetup, skillEnvVar + " does not point to a readable " + skillFile}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", &cliError{exitSetup, "could not determine the user home directory; set " + skillEnvVar}
	}
	candidates := []string{
		filepath.Join(home, ".agents", "skills", skillDirName, skillFile),
		filepath.Join(home, ".codex", "skills", skillDirName, skillFile),
	}
	pluginSkills, _ := filepath.Glob(filepath.Join(home, ".codex", "plugins", "cache", "*", skillDirName, "*", "skills", skillDirName, skillFile))
	sort.Strings(pluginSkills)
	candidates = append(candidates, pluginSkills...)
	for _, candidate := range candidates {
		if isRegularFile(candidate) {
			return candidate, nil
		}
	}
	return "", &cliError{exitSetup, "CodeRabbit Plus skill was not found; install the plugin or set " + skillEnvVar}
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

type tempRefs struct{ base, head string }

func newTempRefs() tempRefs {
	token := make([]byte, 8)
	_, _ = rand.Read(token)
	id := hex.EncodeToString(token)
	return tempRefs{"refs/coderabbit-plus/" + id + "/base", "refs/coderabbit-plus/" + id + "/head"}
}
func (r runner) cleanRefs(refs tempRefs) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, _ = r.run(ctx, "git", "update-ref", "-d", refs.base)
	_, _ = r.run(ctx, "git", "update-ref", "-d", refs.head)
}

func prompt(skill string, rev revision) string {
	return "Review the immutable Git range " + rev.base + ".." + rev.head + " in this disposable worktree. Read and follow the installed skill at " + skill + ". Return only its paste-ready final review. Do not modify files. If there are no issues, return exactly: " + rev.clean
}

func (r runner) review(ctx context.Context, req request, rev revision, skill string) (string, error) {
	worktree, err := os.MkdirTemp("", "coderabbit-plus-")
	if err != nil {
		return "", &cliError{exitSetup, "could not create disposable worktree"}
	}
	added := false
	defer func() {
		if added {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, _ = r.run(cleanupCtx, "git", "worktree", "remove", "--force", worktree)
		}
		_ = os.RemoveAll(worktree)
	}()
	if _, err = r.run(ctx, "git", "worktree", "add", "--detach", worktree, rev.head); err != nil {
		return "", &cliError{exitSetup, "could not create disposable worktree"}
	}
	added = true
	result, err := os.CreateTemp("", "coderabbit-plus-last-message-")
	if err != nil {
		return "", &cliError{exitSetup, "could not create temporary result file"}
	}
	resultFile := result.Name()
	if err := result.Close(); err != nil {
		_ = os.Remove(resultFile)
		return "", &cliError{exitSetup, "could not prepare temporary result file"}
	}
	defer os.Remove(resultFile)
	args := []string{"exec", "--approve-for-me", "--ephemeral", "--json", "-C", worktree, "--model", req.model, "-c", "model_reasoning_effort=" + req.effort, "--output-last-message", resultFile, prompt(skill, rev)}
	cmd := r.command(ctx, "codex", args...)
	cmd.Dir = r.root
	cmd.Stdin = strings.NewReader("")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard
	if err = cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", &cliError{exitCanceled, "review canceled"}
		}
		return "", &cliError{exitCodex, "Codex review failed: " + firstLine(stderr.String(), err.Error())}
	}
	status, statusErr := r.run(ctx, "git", "-C", worktree, "status", "--porcelain=v1", "--untracked-files=all")
	if statusErr != nil {
		return "", &cliError{exitSetup, "could not verify disposable worktree"}
	}
	if strings.TrimSpace(string(status)) != "" {
		return "", &cliError{exitCodex, "Codex modified the disposable worktree"}
	}
	data, err := os.ReadFile(resultFile)
	if err != nil {
		return "", &cliError{exitCodex, "Codex produced no final review"}
	}
	answer := normalizeOutput(string(data))
	if answer == "" {
		return "", &cliError{exitCodex, "Codex produced an empty final review"}
	}
	return answer, nil
}

func firstLine(s, fallback string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		s = fallback
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
func normalizeOutput(s string) string {
	s = strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	if s == "" {
		return ""
	}
	return s + "\n"
}

func execute(ctx context.Context, args []string, out, errOut io.Writer) int {
	req, err := parseArgs(args)
	if err != nil {
		if e, ok := err.(*cliError); ok && e.message == "help" {
			fmt.Fprintln(out, "Usage: coderabbit-plus [--low|--max] <pr <number>|commit <branch>>")
			return 0
		}
		if e, ok := err.(*cliError); ok && e.message == "version" {
			fmt.Fprintln(out, version)
			return 0
		}
		return writeError(errOut, err)
	}
	r := newRunner()
	if err := r.preflight(ctx); err != nil {
		return writeError(errOut, err)
	}
	skill, err := installedSkillPath()
	if err != nil {
		return writeError(errOut, err)
	}
	refs := newTempRefs()
	defer r.cleanRefs(refs)
	var rev revision
	if req.mode == "pr" {
		rev, err = r.resolvePR(ctx, req.value, refs)
	} else {
		rev, err = r.resolveCommit(ctx, req.value, refs)
	}
	if err != nil {
		return writeError(errOut, err)
	}
	answer, err := r.review(ctx, req, rev, skill)
	if err != nil {
		return writeError(errOut, err)
	}
	_, _ = io.WriteString(out, answer)
	return 0
}

func writeError(w io.Writer, err error) int {
	code := exitSetup
	var e *cliError
	if errors.As(err, &e) {
		code = e.code
	}
	fmt.Fprintln(w, firstLine(err.Error(), "review failed"))
	return code
}
func main() { os.Exit(execute(context.Background(), os.Args[1:], os.Stdout, os.Stderr)) }
