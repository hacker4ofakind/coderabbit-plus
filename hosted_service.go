package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type repositoryConfig struct {
	InstallationID int64  `json:"installation_id"`
	RepositoryID   int64  `json:"repository_id"`
	Repo           string `json:"repository"`
	Automatic      bool   `json:"automatic"`
}

type serviceConfig struct {
	AppID           int64              `json:"app_id"`
	AppSlug         string             `json:"app_slug"`
	PrivateKeyFile  string             `json:"private_key_file"`
	WebhookSecret   string             `json:"webhook_secret_file"`
	SpoolDir        string             `json:"spool_dir"`
	CodexHome       string             `json:"codex_home"`
	Listen          string             `json:"listen"`
	Image           string             `json:"image"`
	Depth           string             `json:"depth"`
	DebounceSeconds int                `json:"debounce_seconds"`
	SeccompProfile  string             `json:"seccomp_profile"`
	AppArmorProfile string             `json:"apparmor_profile"`
	Repositories    []repositoryConfig `json:"repositories"`
}

var (
	repoNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9_.-]+$`)
	appSlugRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	imageRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@:-]*$`)
)

func readServiceConfig(path string) (serviceConfig, error) {
	cfg := serviceConfig{Listen: "127.0.0.1:8876", Image: "coderabbit-plus:runner",
		Depth: "default", DebounceSeconds: 45}
	file, err := os.Open(path)
	if err != nil {
		return cfg, err
	}
	defer file.Close()
	dec := json.NewDecoder(file)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("invalid service configuration: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return cfg, errors.New("unexpected data after the service configuration")
	}
	host, _, err := net.SplitHostPort(cfg.Listen)
	if err != nil || host != "127.0.0.1" {
		return cfg, errors.New("listen must be a 127.0.0.1 address with a port")
	}
	if cfg.AppID <= 0 || !appSlugRE.MatchString(cfg.AppSlug) || !imageRE.MatchString(cfg.Image) ||
		len(cfg.Repositories) == 0 || !validDepth(cfg.Depth) || cfg.DebounceSeconds < 0 || cfg.DebounceSeconds > 300 {
		return cfg, errors.New("invalid app identity, image, depth, debounce or repository allowlist")
	}
	for _, path := range []string{cfg.PrivateKeyFile, cfg.WebhookSecret, cfg.SpoolDir, cfg.CodexHome} {
		if !filepath.IsAbs(path) {
			return cfg, errors.New("secret, spool and Codex home paths must be absolute")
		}
	}
	for _, path := range []string{cfg.SpoolDir, cfg.CodexHome} {
		if strings.ContainsAny(path, ",\"\n\r") {
			return cfg, errors.New("Docker mount paths cannot contain commas, quotes or line breaks")
		}
	}
	if cfg.SeccompProfile != "" && !filepath.IsAbs(cfg.SeccompProfile) {
		return cfg, errors.New("seccomp_profile must be an absolute profile file path")
	}
	if cfg.AppArmorProfile == "unconfined" {
		return cfg, errors.New("apparmor_profile must name a confined profile")
	}
	seen := make(map[int64]bool)
	for _, repo := range cfg.Repositories {
		_, name, _ := strings.Cut(repo.Repo, "/")
		if repo.InstallationID <= 0 || repo.RepositoryID <= 0 || !repoNameRE.MatchString(repo.Repo) ||
			name == "." || name == ".." || seen[repo.RepositoryID] {
			return cfg, errors.New("invalid or duplicate repository in allowlist")
		}
		seen[repo.RepositoryID] = true
	}
	return cfg, nil
}

func validDepth(depth string) bool { return depth == "default" || depth == "low" || depth == "max" }
func validMode(mode string) bool {
	return mode == modeFull || mode == modeIncremental || mode == modeCommit
}

type reviewJobSpec struct {
	InstallationID      int64
	RepositoryID        int64
	Repo                string
	PR                  int
	BaseRef, Base, Head string
	Mode                string
}

func (job reviewJobSpec) marker() string {
	data, _ := json.Marshal(job)
	key := sha256.Sum256(data)
	return "<!-- coderabbit-plus:" + hex.EncodeToString(key[:]) + " -->"
}

func (job reviewJobSpec) prKey() string {
	return fmt.Sprintf("%d/%d", job.RepositoryID, job.PR)
}

func (job reviewJobSpec) matchesPR(pr githubPR) bool {
	return pr.Number == job.PR && pr.State == "open" && !pr.Draft && pr.Base.Repo.Private &&
		pr.Base.Repo.ID == job.RepositoryID && pr.Head.Repo.ID == job.RepositoryID &&
		strings.EqualFold(pr.Base.Repo.FullName, job.Repo) &&
		pr.Base.Ref == job.BaseRef && pr.Base.SHA == job.Base && pr.Head.SHA == job.Head
}

type pullRequestEvent struct {
	Action       string           `json:"action"`
	Repository   githubRepository `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	PullRequest githubPR `json:"pull_request"`
	Label       struct {
		Name string `json:"name"`
	} `json:"label"`
	Changes struct {
		Base json.RawMessage `json:"base"`
	} `json:"changes"`
}

func (cfg serviceConfig) allowedRepository(installationID, repoID int64, name string) (repositoryConfig, bool) {
	for _, repo := range cfg.Repositories {
		if repo.InstallationID == installationID && repo.RepositoryID == repoID && strings.EqualFold(repo.Repo, name) {
			return repo, true
		}
	}
	return repositoryConfig{}, false
}

func (cfg serviceConfig) jobForEvent(event pullRequestEvent) (reviewJobSpec, time.Duration, bool) {
	repo, allowed := cfg.allowedRepository(event.Installation.ID, event.Repository.ID, event.Repository.FullName)
	if !allowed || !event.Repository.Private {
		return reviewJobSpec{}, 0, false
	}
	pr := event.PullRequest
	job := reviewJobSpec{InstallationID: repo.InstallationID, RepositoryID: repo.RepositoryID, Repo: repo.Repo,
		PR: pr.Number, BaseRef: pr.Base.Ref, Base: pr.Base.SHA, Head: pr.Head.SHA}
	if !job.valid() || !job.matchesPR(pr) {
		return reviewJobSpec{}, 0, false
	}
	if event.Action == "labeled" {
		switch event.Label.Name {
		case "coderabbit-plus:full":
			job.Mode = modeFull
		case "coderabbit-plus:commit":
			job.Mode = modeCommit
		default:
			return reviewJobSpec{}, 0, false
		}
		return job, 0, true
	}
	if !repo.Automatic {
		return reviewJobSpec{}, 0, false
	}
	switch event.Action {
	case "opened", "reopened", "ready_for_review":
		job.Mode = modeFull
	case "edited":
		if len(event.Changes.Base) == 0 || string(event.Changes.Base) == "null" {
			return reviewJobSpec{}, 0, false
		}
		job.Mode = modeFull
	case "synchronize":
		job.Mode = modeIncremental
	default:
		return reviewJobSpec{}, 0, false
	}
	return job, time.Duration(cfg.DebounceSeconds) * time.Second, true
}

func (job reviewJobSpec) valid() bool {
	return job.PR > 0 && validBranch(job.BaseRef) && validSHA(job.Base) && validSHA(job.Head)
}

type reviewService struct {
	cfg    serviceConfig
	app    githubApp
	secret []byte
	queue  *reviewQueue
}

func serve(ctx context.Context, path string) error {
	cfg, err := readServiceConfig(path)
	if err != nil {
		return err
	}
	for _, tool := range []string{"git", "docker"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s is required on the service host", tool)
		}
	}
	info, err := os.Stat(cfg.CodexHome)
	if err != nil || !info.IsDir() {
		return errors.New("Codex home must be an existing directory")
	}
	if err := os.MkdirAll(cfg.SpoolDir, 0700); err != nil {
		return err
	}
	if err := cfg.checkMounts(); err != nil {
		return err
	}
	app, err := loadGitHubApp(cfg)
	if err != nil {
		return err
	}
	secret, err := os.ReadFile(cfg.WebhookSecret)
	if err != nil {
		return err
	}
	secret = []byte(strings.TrimSpace(string(secret)))
	if len(secret) < 32 {
		return errors.New("webhook secret must be at least 32 bytes")
	}
	queue, err := loadReviewQueue(filepath.Join(cfg.SpoolDir, "queue.json"))
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}

	s := reviewService{cfg: cfg, app: app, secret: secret, queue: queue}
	server := &http.Server{Handler: s.routes(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: 16 << 10}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		s.work(ctx)
	}()
	httpErr := make(chan error, 1)
	go func() { httpErr <- server.Serve(listener) }()
	log.Printf("CodeRabbit Plus listening on %s", listener.Addr())
	select {
	case <-ctx.Done():
	case err = <-httpErr:
		stop()
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdown)
	<-workerDone
	return err
}

func (s *reviewService) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /github/webhook", s.webhook)
	return mux
}

func (s *reviewService) webhook(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, 1<<20))
	if err != nil {
		http.Error(w, "invalid webhook body", http.StatusBadRequest)
		return
	}
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(req.Header.Get("X-Hub-Signature-256")), []byte(expected)) {
		http.Error(w, "invalid webhook signature", http.StatusUnauthorized)
		return
	}
	if req.Header.Get("X-GitHub-Event") != "pull_request" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var event pullRequestEvent
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "invalid pull request event", http.StatusBadRequest)
		return
	}
	job, delay, accept := s.cfg.jobForEvent(event)
	if !accept {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.queue.enqueue(job, time.Now().Add(delay)); err != nil {
		log.Printf("could not queue %s PR #%d: %v", job.Repo, job.PR, err)
		http.Error(w, "review queue unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *reviewService) work(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		job, previous, ok := s.queue.next(time.Now())
		if !ok {
			select {
			case <-ctx.Done():
			case <-ticker.C:
			}
			continue
		}
		runCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
		err := s.process(runCtx, job.Spec, previous, s.runContainer)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil && !errors.Is(err, errSuperseded) {
			log.Printf("%s PR #%d %s review attempt %d: %v", job.Spec.Repo, job.Spec.PR, job.Spec.Mode, job.Attempts+1, err)
		}
		if err := s.queue.finish(job, err, time.Now()); err != nil {
			log.Printf("could not write review queue: %v", err)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
		}
	}
}

func (s *reviewService) process(ctx context.Context, job reviewJobSpec, previous string,
	review func(context.Context, reviewJobSpec, string, string) (reviewOutput, error)) error {
	_, allowed := s.cfg.allowedRepository(job.InstallationID, job.RepositoryID, job.Repo)
	if !allowed || !job.valid() || !validMode(job.Mode) {
		return errSuperseded
	}
	token, err := s.app.installationToken(ctx, job, map[string]string{"contents": "read", "pull_requests": "read"})
	if err != nil {
		return err
	}
	if err := s.app.checkSnapshot(ctx, job, token); err != nil {
		return err
	}
	exists, err := s.app.alreadyPublished(ctx, job, token)
	if err != nil || exists {
		return err
	}
	result, err := review(ctx, job, previous, token)
	if err != nil {
		return err
	}
	writer, err := s.app.installationToken(ctx, job, map[string]string{"pull_requests": "write"})
	if err != nil {
		return err
	}
	if err := s.app.checkSnapshot(ctx, job, writer); err != nil {
		return err
	}
	if err := s.app.publish(ctx, job, writer, result); err != nil {
		return err
	}
	log.Printf("published %s review for %s PR #%d at %s", result.Mode, job.Repo, job.PR, job.Head[:12])
	return nil
}

func (cfg serviceConfig) checkMounts() error {
	home, err := filepath.EvalSymlinks(cfg.CodexHome)
	if err != nil {
		return err
	}
	spool, err := filepath.EvalSymlinks(cfg.SpoolDir)
	if err != nil {
		return err
	}
	if pathWithin(home, spool) || pathWithin(spool, home) {
		return errors.New("Codex home and host spool must be separate directories")
	}
	for _, secret := range []string{cfg.PrivateKeyFile, cfg.WebhookSecret} {
		resolved, err := filepath.EvalSymlinks(secret)
		if err != nil {
			return err
		}
		if pathWithin(home, secret) || pathWithin(home, resolved) {
			return errors.New("GitHub secrets must be outside the mounted Codex home")
		}
	}
	return nil
}

func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
