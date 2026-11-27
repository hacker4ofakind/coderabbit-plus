package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type githubApp struct {
	id   string
	slug string
	key  *rsa.PrivateKey
	http *http.Client
}

type githubRepository struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
}

type githubPRRef struct {
	SHA  string           `json:"sha"`
	Ref  string           `json:"ref"`
	Repo githubRepository `json:"repo"`
}

type githubPR struct {
	Number int         `json:"number"`
	State  string      `json:"state"`
	Draft  bool        `json:"draft"`
	Base   githubPRRef `json:"base"`
	Head   githubPRRef `json:"head"`
}

func loadGitHubApp(cfg serviceConfig) (githubApp, error) {
	app := githubApp{id: strconv.FormatInt(cfg.AppID, 10), slug: cfg.AppSlug,
		http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}}
	keyPEM, err := os.ReadFile(cfg.PrivateKeyFile)
	if err != nil {
		return app, err
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return app, errors.New("GitHub App private key must be an RSA PEM file")
	}
	if block.Type == "RSA PRIVATE KEY" {
		app.key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	} else {
		var raw any
		raw, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		app.key, _ = raw.(*rsa.PrivateKey)
	}
	if err != nil || app.key == nil || app.key.N.BitLen() < 2048 {
		return app, errors.New("invalid GitHub App RSA private key")
	}
	return app, nil
}

func (app githubApp) jwt(now time.Time) (string, error) {
	claims := fmt.Sprintf(`{"iat":%d,"exp":%d,"iss":%q}`, now.Add(-time.Minute).Unix(), now.Add(8*time.Minute).Unix(), app.id)
	encode := base64.RawURLEncoding.EncodeToString
	payload := encode([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + encode([]byte(claims))
	digest := sha256.Sum256([]byte(payload))
	sig, err := rsa.SignPKCS1v15(nil, app.key, crypto.SHA256, digest[:])
	return payload + "." + encode(sig), err
}

func (app githubApp) request(ctx context.Context, method, path, token string, data, result any) (bool, error) {
	var body io.Reader
	if data != nil {
		payload, err := json.Marshal(data)
		if err != nil {
			return false, err
		}
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.github.com"+path, body)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	req.Header.Set("User-Agent", "coderabbit-plus")
	if data != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := app.http.Do(req)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return false, fmt.Errorf("GitHub %s returned HTTP %d", method, res.StatusCode)
	}
	if result != nil {
		if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(result); err != nil {
			return false, errors.New("invalid GitHub API response")
		}
	}
	return strings.Contains(res.Header.Get("Link"), `rel="next"`), nil
}

func (app githubApp) installationToken(ctx context.Context, job reviewJobSpec, permissions map[string]string) (string, error) {
	jwt, err := app.jwt(time.Now())
	if err != nil {
		return "", err
	}
	var result struct {
		Token string `json:"token"`
	}
	_, err = app.request(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", job.InstallationID), jwt,
		map[string]any{"repository_ids": []int64{job.RepositoryID}, "permissions": permissions}, &result)
	if err == nil && result.Token == "" {
		err = errors.New("GitHub did not return an installation token")
	}
	return result.Token, err
}

func (job reviewJobSpec) prPath() string {
	return fmt.Sprintf("/repos/%s/pulls/%d", job.Repo, job.PR)
}

func (app githubApp) getPR(ctx context.Context, job reviewJobSpec, token string) (githubPR, error) {
	var pr githubPR
	_, err := app.request(ctx, http.MethodGet, job.prPath(), token, nil, &pr)
	return pr, err
}

func (app githubApp) checkSnapshot(ctx context.Context, job reviewJobSpec, token string) error {
	pr, err := app.getPR(ctx, job, token)
	if err != nil {
		return err
	}
	if !job.matchesPR(pr) {
		return errSuperseded
	}
	return nil
}

func (app githubApp) alreadyPublished(ctx context.Context, job reviewJobSpec, token string) (bool, error) {
	marker := job.marker()
	for page := 1; page <= 100; page++ {
		var reviews []struct {
			Body     string `json:"body"`
			CommitID string `json:"commit_id"`
			User     struct {
				Login string `json:"login"`
				Type  string `json:"type"`
			} `json:"user"`
		}
		query := "?per_page=50&page=" + strconv.Itoa(page)
		more, err := app.request(ctx, http.MethodGet, job.prPath()+"/reviews"+query, token, nil, &reviews)
		if err != nil {
			return false, err
		}
		for _, review := range reviews {
			if review.CommitID == job.Head && review.User.Type == "Bot" &&
				strings.EqualFold(review.User.Login, app.slug+"[bot]") &&
				strings.HasPrefix(review.Body, marker+"\n") {
				return true, nil
			}
		}
		if !more {
			return false, nil
		}
	}
	return false, errors.New("could not check existing reviews within the pagination limit")
}

func (app githubApp) publish(ctx context.Context, job reviewJobSpec, token string, result reviewOutput) error {
	mode := map[string]string{modeFull: "Full PR", modeIncremental: "Incremental", modeCommit: "Latest commit"}[result.Mode]
	body := fmt.Sprintf("%s\n**CodeRabbit Plus • %s review**\n\nReviewed `%s` → `%s`.\n\n%s",
		job.marker(), mode, result.Base, result.Head, renderHandoff(job, result))
	if len(body) > 65_000 {
		return errors.New("rendered review exceeds GitHub's body size limit")
	}
	_, err := app.request(ctx, http.MethodPost, job.prPath()+"/reviews", token,
		map[string]string{"event": "COMMENT", "commit_id": result.Head, "body": body}, nil)
	return err
}

func renderHandoff(job reviewJobSpec, result reviewOutput) string {
	body := strings.TrimSpace(result.Body)
	prClean := fmt.Sprintf("No issues found in PR #%d.", job.PR)
	commitClean := "No issues found in commit " + job.Head[:12] + "."
	if body == prClean || body == commitClean {
		return body
	}
	if trimmed, ok := strings.CutPrefix(body, "```txt\n"); ok {
		if trimmed, ok := strings.CutSuffix(trimmed, "\n```"); ok {
			body = trimmed
		}
	}
	// Fence model output as a copyable handoff, including any inner code
	// fences, mentions, or Markdown supplied by the reviewed repository.
	longest, run := 2, 0
	for _, ch := range body {
		if ch == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	return fence + "txt\n" + body + "\n" + fence
}
