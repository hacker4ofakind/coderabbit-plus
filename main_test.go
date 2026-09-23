package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestParseArgsUsesDefaultModel(t *testing.T) {
	r, err := parseArgs([]string{"pr", "12"})
	if err != nil || r.model != "gpt-6-sol" || r.effort != "high" {
		t.Fatalf("parseArgs(default) = %#v, %v", r, err)
	}
}

func TestParseArgsAcceptsLowInEitherPosition(t *testing.T) {
	for _, args := range [][]string{{"--low", "pr", "12"}, {"pr", "12", "--low"}} {
		r, err := parseArgs(args)
		if err != nil || r.model != "gpt-6-sol" || r.effort != "low" {
			t.Fatalf("parseArgs(%v) = %#v, %v", args, r, err)
		}
	}
}

func TestParseArgsAcceptsMaxInEitherPosition(t *testing.T) {
	for _, args := range [][]string{{"--max", "pr", "12"}, {"pr", "12", "--max"}} {
		r, err := parseArgs(args)
		if err != nil || r.model != "gpt-6-astra" || r.effort != "max" {
			t.Fatalf("parseArgs(%v) = %#v, %v", args, r, err)
		}
	}
}

func TestParseArgsRejectsLowAndMaxTogether(t *testing.T) {
	for _, args := range [][]string{{"--low", "--max", "pr", "12"}, {"pr", "12", "--max", "--low"}} {
		if _, err := parseArgs(args); err == nil {
			t.Fatalf("expected failure for %v", args)
		}
	}
}

func TestParseArgsRejectsUnsafeInput(t *testing.T) {
	for _, args := range [][]string{{"pr", "0"}, {"pr", "1x"}, {"commit", "bad..name"}, {"commit", "-bad"}, {"pr", "1", "extra"}, {"--wat", "pr", "1"}} {
		if _, err := parseArgs(args); err == nil {
			t.Fatalf("expected failure for %v", args)
		}
	}
}

func TestPromptPinsRangeAndCleanContract(t *testing.T) {
	base, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
	p := prompt("skill", revision{base: base, head: head, clean: "No issues found in PR #7."})
	for _, want := range []string{base + ".." + head, "skill", "No issues found in PR #7."} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt missing %q: %s", want, p)
		}
	}
}

func TestNormalizeOutputExactlyOneTrailingNewline(t *testing.T) {
	if got, want := normalizeOutput("hello\r\n\r\n"), "hello\n"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if got := normalizeOutput(""); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestRemoteAndSHAValidation(t *testing.T) {
	if !isGitHubRemote("git@github.com:owner/repo.git") || isGitHubRemote("https://gitlab.com/a/b") {
		t.Fatal("unexpected remote recognition")
	}
	if !validSHA(strings.Repeat("a", 40)) || validSHA("deadbeef") {
		t.Fatal("unexpected SHA validation")
	}
}

func TestFakePRReviewFlowUsesClosedStdinAndIsolatedWorktree(t *testing.T) {
	var calls [][]string
	r := runner{root: t.TempDir(), lookPath: func(string) (string, error) { return "fake", nil }}
	r.command = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls = append(calls, append([]string{name}, args...))
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcess", "--", name)
		cmd.Args = append(cmd.Args, args...)
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
		return cmd
	}
	refs := newTempRefs()
	rev, err := r.resolvePR(context.Background(), "7", refs)
	if err != nil {
		t.Fatal(err)
	}
	answer, err := r.review(context.Background(), request{model: "gpt-6-sol", effort: "high"}, rev, "skill")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "Inline comments:\n- test\n" {
		t.Fatalf("unexpected output %q", answer)
	}
	var codex []string
	for _, call := range calls {
		if call[0] == "codex" {
			codex = call
			break
		}
	}
	joined := strings.Join(codex, " ")
	for _, want := range []string{"exec", "--approve-for-me", "--ephemeral", "--json", "-C", "--model gpt-6-sol", "model_reasoning_effort=high", "--output-last-message"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Codex invocation missing %q: %v", want, codex)
		}
	}
}

// TestHelperProcess is a fake git/gh/codex executable used without a network or model call.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	args := os.Args
	i := 0
	for ; i < len(args) && args[i] != "--"; i++ {
	}
	if i+1 >= len(args) {
		os.Exit(2)
	}
	program, cmdArgs := args[i+1], args[i+2:]
	switch program {
	case "gh":
		if len(cmdArgs) > 1 && cmdArgs[0] == "pr" && cmdArgs[1] == "view" {
			fmt.Print(`{"baseRefName":"main","baseRefOid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","headRefOid":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`)
		}
	case "git":
		joined := strings.Join(cmdArgs, " ")
		switch {
		case strings.Contains(joined, "rev-parse") && strings.Contains(joined, "/base^{commit}"):
			fmt.Print(strings.Repeat("a", 40))
		case strings.Contains(joined, "rev-parse") && strings.Contains(joined, "/head^{commit}"):
			fmt.Print(strings.Repeat("b", 40))
		case strings.Contains(joined, "merge-base"):
			fmt.Print(strings.Repeat("c", 40))
		}
	case "codex":
		for j, arg := range cmdArgs {
			if arg == "--output-last-message" && j+1 < len(cmdArgs) {
				_ = os.WriteFile(cmdArgs[j+1], []byte("Inline comments:\r\n- test\r\n"), 0600)
				break
			}
		}
	}
	os.Exit(0)
}
