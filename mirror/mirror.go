/*
Copyright 2015 Google Inc. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package mirror provides two-way synchronization of metadata stored in git-notes and Phabricator.
package mirror

import (
	"github.com/akatrevorjay/git-appraise/repository"
	"github.com/akatrevorjay/git-appraise/review"
	"github.com/akatrevorjay/git-appraise/review/comment"
	"github.com/akatrevorjay/git-phabricator-mirror/mirror/arcanist"
	review_utils "github.com/akatrevorjay/git-phabricator-mirror/mirror/review"
)

var arc = arcanist.Arcanist{}

// processedStates is used to keep track of the state of each repository at the last time we processed it.
// That, in turn, is used to avoid re-processing a repo if its state has not changed.
var processedStates = make(map[string]string)
var existingComments = make(map[string][]review.CommentThread)
var openReviews = make(map[string][]review_utils.PhabricatorReview)

func hasOverlap(newComment comment.Comment, existingComments []review.CommentThread) bool {
	for _, existing := range existingComments {
		if review_utils.Overlaps(newComment, existing.Comment) {
			return true
		} else if hasOverlap(newComment, existing.Children) {
			return true
		}
	}
	return false
}

func mirrorRepoToReview(repo repository.Repo, tool review_utils.Tool, syncToRemote bool) {
	logger.Infof("Start repo=%s tool=%s syncToRemote=%s", repo, tool, syncToRemote)

	if syncToRemote {
		repo.PullNotes("origin", "refs/notes/devtools/*")
	}

	stateHash, err := repo.GetRepoStateHash()
	if err != nil {
		orPanic(err)
	}
	if processedStates[repo.GetPath()] != stateHash {
		logger.Infof("Mirroring repo: %s", repo)
		for _, r := range review.ListAll(repo) {
			reviewJson, err := r.GetJSON()
			if err != nil {
				orPanic(err)
			}
			logger.Infof("Mirroring review: %s", reviewJson)
			existingComments[r.Revision] = r.Comments
			reviewDetails, err := r.Details()
			if err == nil {
				tool.EnsureRequestExists(repo, *reviewDetails)
			}
		}
		openReviews[repo.GetPath()] = tool.ListOpenReviews(repo)
		processedStates[repo.GetPath()] = stateHash
		tool.Refresh(repo)
	}

ReviewLoop:
	for _, phabricatorReview := range openReviews[repo.GetPath()] {
		if reviewCommit := phabricatorReview.GetFirstCommit(repo); reviewCommit != "" {
			logger.Infof("Processing review: %s", reviewCommit)
			r, err := review.GetSummary(repo, reviewCommit)
			if err != nil {
				orPanic(err)
			} else if r == nil {
				logger.Infof("Skipping unknown review %q", reviewCommit)
				continue ReviewLoop
			}
			revisionComments := existingComments[reviewCommit]
			logger.Infof("Loaded %d comments for %v\n", len(revisionComments), reviewCommit)
			for _, c := range phabricatorReview.LoadComments() {
				if !hasOverlap(c, revisionComments) {
					// The comment is new.
					note, err := c.Write()
					if err != nil {
						orPanic(err)
					}
					logger.Infof("Appending a comment: %s", string(note))
					repo.AppendNote(comment.Ref, reviewCommit, note)
				} else {
					logger.Infof("Skipping '%v', as it has already been written\n", c)
				}
			}
		}
	}
	if syncToRemote {
		if err := repo.PushNotes("origin", "refs/notes/devtools/*"); err != nil {
			logger.Errorf("Failed to push updates to the repo %v: %v\n", repo, err)
		}
	}
}

// Repo mirrors the given repository using the system-wide installation of
// the "arcanist" command line tool.
func Repo(repo repository.Repo, syncToRemote bool) {
	arc.Refresh(repo)
	mirrorRepoToReview(repo, arc, syncToRemote)
}
