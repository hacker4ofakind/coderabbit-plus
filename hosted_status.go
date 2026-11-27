package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type reviewPhase string

const (
	reviewQueued     reviewPhase = "queued"
	reviewRunning    reviewPhase = "running"
	reviewPosted     reviewPhase = "posted"
	reviewRetrying   reviewPhase = "retrying"
	reviewFailed     reviewPhase = "failed"
	reviewSuperseded reviewPhase = "superseded"
	statusMarker                 = "<!-- coderabbit-plus-review-status -->"
)

type reviewNotice struct {
	Job      reviewJobSpec
	Phase    reviewPhase
	Due      time.Time
	Attempts int
}

func (s *reviewService) workNotices(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if notice, ok := s.queue.nextNotice(time.Now()); ok {
			err := s.sendNotice(ctx, notice)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				log.Printf("%s PR #%d status update failed: %v", notice.Job.Repo, notice.Job.PR, err)
			}
			if err := s.queue.finishNotice(notice, err, time.Now()); err == nil {
				continue
			} else {
				log.Printf("could not write review status queue: %v", err)
			}
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}

func (s *reviewService) sendNotice(ctx context.Context, notice reviewNotice) error {
	job := notice.Job
	if _, allowed := s.cfg.allowedRepository(job.InstallationID, job.RepositoryID, job.Repo); !allowed ||
		!job.valid() || !validMode(job.Mode) {
		return nil
	}
	noticeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	token, err := s.app.installationToken(noticeCtx, job, map[string]string{"pull_requests": "write"})
	if err != nil {
		return err
	}
	pr, err := s.app.getPR(noticeCtx, job, token)
	if err != nil {
		return err
	}
	if pr.Number != job.PR || !pr.Base.Repo.Private || pr.Base.Repo.ID != job.RepositoryID ||
		!strings.EqualFold(pr.Base.Repo.FullName, job.Repo) ||
		(!job.matchesPR(pr) && notice.Phase != reviewSuperseded) {
		return nil
	}
	return s.app.upsertStatus(noticeCtx, notice, token)
}

func statusBody(job reviewJobSpec, phase reviewPhase) string {
	message := map[reviewPhase]string{
		reviewQueued:     "Review queued",
		reviewRunning:    "Review in progress",
		reviewPosted:     "Review posted",
		reviewRetrying:   "Review will retry",
		reviewFailed:     "Review failed",
		reviewSuperseded: "Review skipped because the PR changed",
	}[phase]
	return fmt.Sprintf("%s\n%s\n**CodeRabbit Plus · %s** for commit `%s`.",
		statusMarker, job.marker(), message, job.Head[:12])
}

func (app githubApp) upsertStatus(ctx context.Context, notice reviewNotice, token string) error {
	job := notice.Job
	path := fmt.Sprintf("/repos/%s/issues/%d/comments", job.Repo, job.PR)
	body := statusBody(job, notice.Phase)
	payload := map[string]string{"body": body}
	for page := 1; page <= 100; page++ {
		var comments []struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
			User struct {
				Login string `json:"login"`
				Type  string `json:"type"`
			} `json:"user"`
		}
		more, err := app.request(ctx, http.MethodGet, path+"?per_page=50&page="+strconv.Itoa(page), token, nil, &comments)
		if err != nil {
			return err
		}
		for _, comment := range comments {
			if comment.ID <= 0 || comment.User.Type != "Bot" || !strings.EqualFold(comment.User.Login, app.slug+"[bot]") ||
				!strings.HasPrefix(comment.Body, statusMarker+"\n") {
				continue
			}
			sameJob := strings.HasPrefix(comment.Body, statusMarker+"\n"+job.marker()+"\n")
			if comment.Body == body || comment.Body == statusBody(job, reviewPosted) ||
				(notice.Phase == reviewSuperseded && !sameJob) {
				return nil
			}
			_, err := app.request(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/comments/%d", job.Repo, comment.ID), token, payload, nil)
			return err
		}
		if !more {
			if notice.Phase == reviewSuperseded {
				return nil
			}
			_, err := app.request(ctx, http.MethodPost, path, token, payload, nil)
			return err
		}
	}
	return errors.New("could not check existing status comments within the pagination limit")
}
