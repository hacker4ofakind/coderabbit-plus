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
	Status  []reviewNotice
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
	state.setNotice(spec, reviewQueued, time.Now())
	return q.save(state)
}

func (q *reviewQueue) start(job queuedReview, now time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	state := q.state
	state.setNotice(job.Spec, reviewRunning, now)
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
	case outcome == nil:
		state.setNotice(job.Spec, reviewPosted, now)
		if job.Spec.Mode != modeCommit {
			state.Last = make(map[string]reviewedHead, len(q.state.Last)+1)
			for key, value := range q.state.Last {
				state.Last[key] = value
			}
			state.Last[job.Spec.prKey()] = reviewedHead{job.Spec.BaseRef, job.Spec.Head}
		}
	case errors.Is(outcome, errSuperseded):
		state.setNotice(job.Spec, reviewSuperseded, now)
	case job.Attempts < 2:
		job.Attempts++
		job.Due = now.Add(time.Duration(job.Attempts) * time.Minute)
		state.Pending = append(state.Pending, job)
		state.setNotice(job.Spec, reviewRetrying, now)
	default:
		state.setNotice(job.Spec, reviewFailed, now)
	}
	return q.save(state)
}

func (state *reviewQueueState) setNotice(spec reviewJobSpec, phase reviewPhase, due time.Time) {
	notices := make([]reviewNotice, 0, len(state.Status)+1)
	for _, notice := range state.Status {
		if notice.Job != spec {
			notices = append(notices, notice)
		}
	}
	// A GitHub outage must not let status notices grow without bound.
	if len(notices) >= 256 {
		notices = notices[1:]
	}
	state.Status = append(notices, reviewNotice{Job: spec, Phase: phase, Due: due})
}

func (q *reviewQueue) nextNotice(now time.Time) (reviewNotice, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, notice := range q.state.Status {
		if !notice.Due.After(now) {
			return notice, true
		}
	}
	return reviewNotice{}, false
}

func (q *reviewQueue) finishNotice(sent reviewNotice, outcome error, now time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, notice := range q.state.Status {
		if notice != sent {
			continue
		}
		state := q.state
		state.Status = append([]reviewNotice(nil), state.Status...)
		if outcome != nil && sent.Attempts < 2 {
			sent.Attempts++
			sent.Due = now.Add(10 * time.Second)
			state.Status[i] = sent
		} else {
			state.Status = append(state.Status[:i], state.Status[i+1:]...)
		}
		return q.save(state)
	}
	// A new phase was recorded while this notice was being sent.
	return nil
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
