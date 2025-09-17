package main

import (
	"context"
	"errors"
	"fmt"
	"log"
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
	ctx context.Context,
	webRoot string,
	repoURL string,
	branch string,
) UpdateResult {
	var fetchManifest, oldManifest, newManifest *Manifest
	var err error

	log.Println("update:", webRoot, repoURL, branch)

	outcome := UpdateError
	fetchManifest, err = FetchRepository(ctx, repoURL, branch)
	if errors.Is(err, context.DeadlineExceeded) {
		outcome = UpdateTimeout
		err = fmt.Errorf("update timeout")
	} else if err == nil {
		oldManifest, _ = backend.GetManifest(webRoot)
		newManifest, err = StoreManifest(webRoot, fetchManifest)
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
