package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type queuedReview struct {
	Spec     reviewJobSpec
	Due      time.Time
	Attempts int
}

type reviewedHead struct {
	BaseRef string
	Head    string
}

type reviewQueueState struct {
	Pending []queuedReview
	Last    map[string]reviewedHead
}

type reviewQueue struct {
	mu    sync.Mutex
	path  string
	state reviewQueueState
}

func loadReviewQueue(path string) (*reviewQueue, error) {
	q := &reviewQueue{path: path}
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &q.state); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return q, nil
}

func (q *reviewQueue) enqueue(spec reviewJobSpec, due time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, job := range q.state.Pending {
		if job.Spec == spec {
			return nil
		}
	}
	if len(q.state.Pending) >= 128 {
		return errors.New("too many pending reviews")
	}
	state := q.state
	state.Pending = append(append([]queuedReview(nil), state.Pending...), queuedReview{Spec: spec, Due: due})
	return q.save(state)
}

func (q *reviewQueue) next(now time.Time) (queuedReview, string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var next queuedReview
	found := false
	for _, job := range q.state.Pending {
		if !job.Due.After(now) && (!found || job.Due.Before(next.Due)) {
			next, found = job, true
		}
	}
	last := q.state.Last[next.Spec.prKey()]
	if last.BaseRef != next.Spec.BaseRef {
		return next, "", found
	}
	return next, last.Head, found
}

func (q *reviewQueue) finish(job queuedReview, outcome error, now time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	state := q.state
	state.Pending = make([]queuedReview, 0, len(q.state.Pending))
	for _, pending := range q.state.Pending {
		if pending.Spec != job.Spec {
			state.Pending = append(state.Pending, pending)
		}
	}
	switch {
	case outcome == nil && job.Spec.Mode != modeCommit:
		state.Last = make(map[string]reviewedHead, len(q.state.Last)+1)
		for key, value := range q.state.Last {
			state.Last[key] = value
		}
		state.Last[job.Spec.prKey()] = reviewedHead{job.Spec.BaseRef, job.Spec.Head}
	case outcome != nil && !errors.Is(outcome, errSuperseded) && job.Attempts < 2:
		job.Attempts++
		job.Due = now.Add(time.Duration(job.Attempts) * time.Minute)
		state.Pending = append(state.Pending, job)
	}
	return q.save(state)
}

// save uses an atomic rename so an interrupted job stays queued on restart.
// The bot's review marker handles a restart after GitHub accepted the post.
func (q *reviewQueue) save(state reviewQueueState) error {
	file, err := os.CreateTemp(filepath.Dir(q.path), "queue-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := json.NewEncoder(file).Encode(state); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), q.path); err != nil {
		return err
	}
	q.state = state
	return nil
}
