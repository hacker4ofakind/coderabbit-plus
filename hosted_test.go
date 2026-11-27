package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func reviewFixture() (serviceConfig, pullRequestEvent) {
	repo := githubRepository{ID: 27, FullName: "owner/repo", Private: true}
	pr := githubPR{Number: 12, State: "open",
		Base: githubPRRef{SHA: strings.Repeat("a", 40), Ref: "main", Repo: repo},
		Head: githubPRRef{SHA: strings.Repeat("b", 40), Ref: "feature", Repo: repo}}
	cfg := serviceConfig{DebounceSeconds: 45, Repositories: []repositoryConfig{{913, 27, repo.FullName, true}}}
	event := pullRequestEvent{Action: "synchronize", Repository: repo, PullRequest: pr}
	event.Installation.ID = 913
	return cfg, event
}

func TestWebhookQueueAndAllowlist(t *testing.T) {
	cfg, event := reviewFixture()
	path := filepath.Join(t.TempDir(), "queue.json")
	queue, err := loadReviewQueue(path)
	if err != nil {
		t.Fatal(err)
	}
	service := reviewService{cfg: cfg, queue: queue, secret: []byte("test signing secret")}
	send := func(event pullRequestEvent, signed bool) int {
		body, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", "/github/webhook", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "pull_request")
		if signed {
			mac := hmac.New(sha256.New, service.secret)
			mac.Write(body)
			req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		}
		w := httptest.NewRecorder()
		service.routes().ServeHTTP(w, req)
		return w.Code
	}
	if got := send(event, false); got != http.StatusUnauthorized {
		t.Fatalf("unsigned delivery returned %d", got)
	}
	fork := event
	fork.PullRequest.Head.Repo.ID = 99
	unlisted := event
	unlisted.Installation.ID++
	public := event
	public.PullRequest.Base.Repo.Private = false
	for _, ignored := range []pullRequestEvent{fork, public, unlisted} {
		if got := send(ignored, true); got != http.StatusNoContent {
			t.Fatalf("out-of-scope delivery returned %d", got)
		}
	}
	for range 2 {
		if got := send(event, true); got != http.StatusAccepted {
			t.Fatalf("allowed delivery returned %d", got)
		}
	}
	reloaded, err := loadReviewQueue(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.state.Pending) != 1 {
		t.Fatalf("expected one persisted review, got %d", len(reloaded.state.Pending))
	}
	if _, _, ok := reloaded.next(time.Now()); ok {
		t.Fatal("synchronize review ran before debounce")
	}
	if notice, ok := reloaded.nextNotice(time.Now()); !ok || notice.Phase != reviewQueued || notice.Job.Head != event.PullRequest.Head.SHA {
		t.Fatalf("review should report its status during debounce: %#v", notice)
	}
	if job, _, ok := reloaded.next(time.Now().Add(time.Minute)); !ok || job.Spec.Mode != modeIncremental || job.Spec.Head != event.PullRequest.Head.SHA {
		t.Fatalf("unexpected queued review: %#v", job)
	}
	cfg.Repositories[0].Automatic = false
	event.Action, event.Label.Name = "labeled", "coderabbit-plus:commit"
	if job, delay, ok := cfg.jobForEvent(event); !ok || delay != 0 || job.Mode != modeCommit {
		t.Fatalf("explicit latest-commit request was ignored: %#v", job)
	}
}

func TestCommitReviewDoesNotAdvanceIncrementalBaseline(t *testing.T) {
	cfg, event := reviewFixture()
	spec, _, _ := cfg.jobForEvent(event)
	q, err := loadReviewQueue(filepath.Join(t.TempDir(), "queue.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	previous := spec.Head
	for _, mode := range []string{modeFull, modeCommit, modeIncremental} {
		spec.Mode = mode
		if mode != modeFull {
			spec.Head = strings.Repeat("c", 40)
		}
		if err := q.enqueue(spec, now); err != nil {
			t.Fatal(err)
		}
		job, baseline, ok := q.next(now)
		if !ok || job.Spec != spec || (mode != modeFull && baseline != previous) {
			t.Fatalf("%s returned %#v and %q", mode, job, baseline)
		}
		if err := q.finish(job, nil, now); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHostedRangeUsesGitHistory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	r := newRunner()
	r.root = t.TempDir()
	ctx := context.Background()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = r.root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "main", "--template=")
	commit := func(message string) string {
		git("-c", "user.name=Review Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false",
			"commit", "--allow-empty", "-m", message)
		return git("rev-parse", "HEAD")
	}
	base := commit("baseline")
	git("checkout", "-b", "previous")
	previous := commit("already reviewed")
	head := commit("new push")
	git("checkout", "-b", "rewritten", base)
	rewritten := commit("force-pushed")
	for _, tc := range []struct{ wantMode, wantBase, mode, head string }{
		{modeFull, base, modeFull, head},
		{modeIncremental, previous, modeIncremental, head},
		{modeFull, base, modeIncremental, rewritten},
		{modeCommit, previous, modeCommit, head},
	} {
		mode, rev, err := r.hostedRange(ctx, reviewInput{Mode: tc.mode, Base: base, Head: tc.head, Previous: previous, PR: 12})
		if err != nil || mode != tc.wantMode || rev.base != tc.wantBase || rev.head != tc.head {
			t.Fatalf("range %#v returned %q %#v %v", tc, mode, rev, err)
		}
	}
}

type testTransport func(*http.Request) (*http.Response, error)

func (fn testTransport) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func TestStatusCommentReusesOwnedCommentAndIgnoresOldJobs(t *testing.T) {
	cfg, event := reviewFixture()
	job, _, _ := cfg.jobForEvent(event)
	newer := job
	newer.Head = strings.Repeat("c", 40)
	var commentBody string
	var creates, updates int
	app := githubApp{slug: "reviewer"}
	app.http = &http.Client{Transport: testTransport(func(req *http.Request) (*http.Response, error) {
		response := func(value any) (*http.Response, error) {
			data, _ := json.Marshal(value)
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data))}, nil
		}
		switch req.Method + " " + req.URL.Path {
		case "GET /repos/owner/repo/issues/12/comments":
			comments := []any{map[string]any{"id": 7, "body": statusBody(job, reviewQueued),
				"user": map[string]string{"login": "someone", "type": "User"}}}
			if commentBody != "" {
				comments = append(comments, map[string]any{"id": 8, "body": commentBody,
					"user": map[string]string{"login": "reviewer[bot]", "type": "Bot"}})
			}
			return response(comments)
		case "POST /repos/owner/repo/issues/12/comments", "PATCH /repos/owner/repo/issues/comments/8":
			var body map[string]string
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			commentBody = body["body"]
			if req.Method == http.MethodPost {
				creates++
			} else {
				updates++
			}
			return response(nil)
		default:
			return nil, fmt.Errorf("unexpected %s %s", req.Method, req.URL.Path)
		}
	})}
	for _, notice := range []reviewNotice{
		{Job: job, Phase: reviewQueued},
		{Job: job, Phase: reviewRunning},
		{Job: job, Phase: reviewPosted},
		{Job: job, Phase: reviewQueued}, // redelivery after posting
		{Job: newer, Phase: reviewQueued},
		{Job: job, Phase: reviewSuperseded},
	} {
		if err := app.upsertStatus(context.Background(), notice, "fake-token"); err != nil {
			t.Fatal(err)
		}
	}
	if creates != 1 || updates != 3 || commentBody != statusBody(newer, reviewQueued) {
		t.Fatalf("expected one comment for latest job, got %d creates, %d updates, and %q", creates, updates, commentBody)
	}
}

func TestFinishingStatusDoesNotDropNewPhase(t *testing.T) {
	cfg, event := reviewFixture()
	job, _, _ := cfg.jobForEvent(event)
	q, err := loadReviewQueue(filepath.Join(t.TempDir(), "queue.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Second)
	if err := q.enqueue(job, now); err != nil {
		t.Fatal(err)
	}
	sent, ok := q.nextNotice(now)
	if !ok || sent.Phase != reviewQueued {
		t.Fatalf("expected the queued status, got %#v", sent)
	}
	running, _, ok := q.next(now)
	if !ok {
		t.Fatal("no review available")
	}
	if err := q.start(running, now); err != nil {
		t.Fatal(err)
	}
	if err := q.finishNotice(sent, nil, now); err != nil {
		t.Fatal(err)
	}
	if current, ok := q.nextNotice(now); !ok || current.Phase != reviewRunning {
		t.Fatalf("queued status receipt overwrote newer running status: %#v", current)
	}
	if err := q.finish(running, nil, now); err != nil {
		t.Fatal(err)
	}
	if current, ok := q.nextNotice(now); !ok || current.Phase != reviewPosted {
		t.Fatalf("expected a posted status after the review finished: %#v", current)
	}
}

func TestRenderHandoffKeepsInnerMarkdownInert(t *testing.T) {
	job := reviewJobSpec{PR: 12, Head: strings.Repeat("b", 40)}
	body := "```txt\nReview @some-user\n```shell\npython check.py\n```\n```"
	want := "````txt\nReview @some-user\n```shell\npython check.py\n```\n````"
	if got := renderHandoff(job, reviewOutput{Body: body}); got != want {
		t.Fatalf("handoff escaped its fence: %q", got)
	}
}

func TestServicePinsAndPublishesOnlyCurrentSnapshot(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"posted", "stale", "redelivery"} {
		t.Run(scenario, func(t *testing.T) {
			cfg, event := reviewFixture()
			job, _, _ := cfg.jobForEvent(event)
			var runs, publications int
			app := githubApp{id: "42", slug: "reviewer", key: key}
			app.http = &http.Client{Transport: testTransport(func(req *http.Request) (*http.Response, error) {
				send := func(value any) (*http.Response, error) {
					data, _ := json.Marshal(value)
					return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data))}, nil
				}
				auth := req.Header.Get("Authorization")
				switch req.Method + " " + req.URL.Path {
				case "POST /app/installations/913/access_tokens":
					var data struct {
						IDs         []int64           `json:"repository_ids"`
						Permissions map[string]string `json:"permissions"`
					}
					if err := json.NewDecoder(req.Body).Decode(&data); err != nil || len(data.IDs) != 1 || data.IDs[0] != job.RepositoryID ||
						strings.Count(auth, ".") != 2 {
						t.Fatalf("bad app token request: %#v, %v", data, err)
					}
					perms := data.Permissions
					switch {
					case len(perms) == 2 && perms["contents"] == "read" && perms["pull_requests"] == "read":
						return send(map[string]string{"token": "read"})
					case len(perms) == 1 && perms["pull_requests"] == "write":
						return send(map[string]string{"token": "write"})
					default:
						t.Fatalf("unexpected installation permissions: %#v", perms)
					}
				case "GET " + job.prPath():
					pr := event.PullRequest
					if scenario == "stale" && runs > 0 {
						pr.Head.SHA = strings.Repeat("c", 40)
					}
					return send(pr)
				case "GET " + job.prPath() + "/reviews":
					if scenario == "redelivery" {
						return send([]any{map[string]any{"body": job.marker() + "\nreview", "commit_id": job.Head,
							"user": map[string]string{"login": "reviewer[bot]", "type": "Bot"}}})
					}
					return send([]any{})
				case "POST " + job.prPath() + "/reviews":
					publications++
					var data map[string]string
					if err := json.NewDecoder(req.Body).Decode(&data); err != nil || auth != "Bearer write" ||
						data["commit_id"] != job.Head || data["event"] != "COMMENT" ||
						!strings.Contains(data["body"], job.marker()) || !strings.Contains(data["body"], job.Base) {
						t.Fatalf("bad GitHub review: %#v, %v", data, err)
					}
					return send(nil)
				}
				return nil, fmt.Errorf("unexpected %s %s", req.Method, req.URL.Path)
			})}
			s := reviewService{cfg: cfg, app: app}
			err := s.process(context.Background(), job, "", func(_ context.Context, requested reviewJobSpec, previous, token string) (reviewOutput, error) {
				runs++
				if requested != job || token != "read" || previous != "" {
					t.Fatalf("wrong review inputs: %#v %q %q", requested, previous, token)
				}
				return reviewOutput{modeFull, job.Base, job.Head, "No issues found in PR #12."}, nil
			})
			switch scenario {
			case "posted":
				if err != nil || runs != 1 || publications != 1 {
					t.Fatalf("publish failed: %v, runs=%d, posts=%d", err, runs, publications)
				}
			case "stale":
				if !errors.Is(err, errSuperseded) || runs != 1 || publications != 0 {
					t.Fatalf("stale review was not discarded: %v, runs=%d, posts=%d", err, runs, publications)
				}
			case "redelivery":
				if err != nil || runs != 0 || publications != 0 {
					t.Fatalf("duplicate was reprocessed: %v, runs=%d, posts=%d", err, runs, publications)
				}
			}
		})
	}
}
