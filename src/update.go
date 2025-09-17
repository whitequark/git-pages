package main

import (
	"fmt"
	"log"
	"time"
)

type UpdateOutcome int

const (
	UpdateError UpdateOutcome = iota
	UpdateTimeout
	UpdateCreated
	UpdateReplaced
	UpdateNoChange
)

type UpdateResult struct {
	outcome  UpdateOutcome
	manifest *Manifest
	err      error
}

func Update(
	webRoot string,
	repoURL string,
	branch string,
) UpdateResult {
	var fetchManifest, oldManifest, newManifest *Manifest
	var err error

	log.Println("update:", webRoot, repoURL, branch)

	outcome := UpdateError
	fetchManifest, err = FetchRepository(repoURL, branch)
	if err == nil {
		oldManifest, _ = backend.GetManifest(webRoot)
		newManifest, err = StoreManifest(backend, webRoot, fetchManifest)
		if err == nil {
			if oldManifest == nil {
				outcome = UpdateCreated
			} else if CompareManifest(oldManifest, newManifest) {
				outcome = UpdateNoChange
			} else {
				outcome = UpdateReplaced
			}
		}
	}

	if err == nil {
		status := ""
		switch outcome {
		case UpdateCreated:
			status = "created"
		case UpdateReplaced:
			status = "replaced"
		case UpdateNoChange:
			status = "unchanged"
		}
		log.Printf("update ok: %s %s %s", webRoot, newManifest.Commit, status)
	} else {
		log.Printf("update err: %s %s", webRoot, err)
	}

	return UpdateResult{outcome, newManifest, err}
}

func UpdateWithTimeout(
	webRoot string,
	repoURL string,
	branch string,
	timeout time.Duration,
) UpdateResult {
	c := make(chan UpdateResult, 1)
	go func() {
		result := Update(webRoot, repoURL, branch)
		c <- result
	}()
	select {
	case result := <-c:
		return result
	case <-time.After(timeout):
		return UpdateResult{outcome: UpdateTimeout, err: fmt.Errorf("update timeout")}
	}
}
