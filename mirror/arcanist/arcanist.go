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

// Package arcanist contains methods for issuing API calls to Phabricator via the "arc" command-line tool.
package arcanist

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/akatrevorjay/git-appraise/repository"
	"github.com/akatrevorjay/git-appraise/review"
	"github.com/akatrevorjay/git-appraise/review/analyses"
	"github.com/akatrevorjay/git-appraise/review/ci"
	"github.com/akatrevorjay/git-appraise/review/comment"
	"github.com/akatrevorjay/git-appraise/review/request"
	review_utils "github.com/akatrevorjay/git-phabricator-mirror/mirror/review"
	"log"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// commitHashType is a special string that Phabricator uses internally to distinguish
// hashes for commit objects from hashes for other types of objects (such as trees or file blobs).
const commitHashType = "gtcm"

// Differential (Phabricator's code review tool) limits review titles to 40 characters.
const differentialTitleLengthLimit = 256

// Differential stores review status as a string, so we have to maintain a mapping of
// the status strings that we care about.
const (
	differentialNeedsReviewStatus = "0"
	differentialClosedStatus      = "3"
	differentialAbandonedStatus   = "4"
)

// defaultRepoDirPrefix is the default parent directory Phabricator uses to store repos.
const defaultRepoDirPrefix = "/var/repo/"

// arcanistRequestTimeout is the amount of time we allow arcanist requests to wait before interrupting them.
const arcanistRequestTimeout = 1 * time.Minute

// unitDiffPropertyName is the name of the property that Phabricator uses for storing the unit test
// results for a given Differential diff
const (
	unitDiffPropertyName = "arc:unit"
	lintDiffPropertyName = "arc:lint"
)

// Arcanist represents an instance of the "arcanist" command-line tool.
type Arcanist struct {
}

// Filter processing of previously closed revisions.
var closedRevisionsMap = make(map[string]bool)

// runArcCommandOrDie runs the given Conduit API call using the "arc" command line tool.
//
// Any errors that could occur here would be a sign of something being seriously
// wrong, so they are treated as fatal. This makes it more evident that something
// has gone wrong when the command is manually run by a user, and gives further
// operations a clean-slate when this is run by supervisord with automatic restarts.
func runArcCommandOrDie(method string, request interface{}, response interface{}) {
	cmd := exec.Command("arc", "call-conduit", method)
	input, err := json.Marshal(request)
	if err != nil {
		log.Panic(err)
	}
	log.Print("Running conduit request: ", method, string(input))
	cmd.Stdin = strings.NewReader(string(input))

	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Start(); err != nil {
		log.Panic(err)
	}
	go func() {
		time.Sleep(arcanistRequestTimeout)
		cmd.Process.Kill()
	}()
	if err := cmd.Wait(); err != nil {
		log.Print("arc", "call-conduit", method, string(input), stdout.String())
		log.Panic(err)
	}
	log.Print("Received conduit response ", stdout.String())
	if err = json.Unmarshal(stdout.Bytes(), response); err != nil {
		log.Panic(err)
	}
}

func abbreviateRefName(ref string) string {
	if strings.HasPrefix(ref, "refs/heads/") {
		return ref[len("refs/heads/"):]
	}
	return ref
}

type DifferentialReview struct {
	ID         string     `json:"id,omitempty"`
	PHID       string     `json:"phid,omitempty"`
	Title      string     `json:"title,omitempty"`
	Branch     string     `json:"branch,omitempty"`
	Status     string     `json:"status,omitempty"`
	StatusName string     `json:"statusName,omitempty"`
	AuthorPHID string     `json:"authorPHID,omitempty"`
	Reviewers  []string   `json:"reviewers,omitempty"`
	Hashes     [][]string `json:"hashes,omitempty"`
	Diffs      []string   `json:"diffs,omitempty"`
}

// GetFirstCommit returns the first commit that is included in the review
func (r DifferentialReview) GetFirstCommit(repo repository.Repo) string {
	var commits []string
	for _, hashPair := range r.Hashes {
		// We only care about the hashes for commits, which have exactly two
		// elements, the first of which is "gtcm".
		if len(hashPair) == 2 && hashPair[0] == commitHashType {
			commits = append(commits, hashPair[1])
		}
	}
	var commitTimestamps []int
	commitsByTimestamp := make(map[int]string)
	for _, commit := range commits {
		if _, err := repo.GetLastParent(commit); err == nil {
			timeString, err1 := repo.GetCommitTime(commit)
			timestamp, err2 := strconv.Atoi(timeString)
			if err1 == nil && err2 == nil {
				commitTimestamps = append(commitTimestamps, timestamp)
				// If there are multiple, equally old commits, then the last one wins.
				commitsByTimestamp[timestamp] = commit
			}
		}
	}
	if len(commitTimestamps) == 0 {
		return ""
	}
	sort.Ints(commitTimestamps)
	revision := commitsByTimestamp[commitTimestamps[0]]
	return revision
}

// queryRequest specifies filters for review queries. Specifically, CommitHashes filters
// reviews to only those that contain the specified hashes, and Status filters reviews to
// only those that match the given status (e.g. "status-any", "status-open", etc.)
type queryRequest struct {
	CommitHashes [][]string `json:"commitHashes,omitempty"`
	Status       string     `json:"status,omitempty"`
}

type queryResponse struct {
	Error        string               `json:"error,omitempty"`
	ErrorMessage string               `json:"errorMessage,omitempty"`
	Response     []DifferentialReview `json:"response,omitempty"`
}

func (arc Arcanist) listDifferentialReviewsOrDie(revision string) []DifferentialReview {
	request := queryRequest{
		CommitHashes: [][]string{[]string{commitHashType, revision}},
	}
	var response queryResponse
	runArcCommandOrDie("differential.query", request, &response)
	return response.Response
}

func (arc Arcanist) ListOpenReviews(repo repository.Repo) []review_utils.PhabricatorReview {
	// TODO(ojarjur): Filter the query by the repo.
	// As is, we simply return all open reviews for *any* repo, and then filter in
	// the calling level.
	request := queryRequest{
		Status: "status-open",
	}
	var response queryResponse
	runArcCommandOrDie("differential.query", request, &response)
	var reviews []review_utils.PhabricatorReview
	for _, r := range response.Response {
		reviews = append(reviews, r)
	}
	return reviews
}

type revisionFields struct {
	Title     string   `json:"title,omitempty"`
	Summary   string   `json:"summary,omitempty"`
	Reviewers []string `json:"reviewerPHIDs,omitempty"`
	CCs       []string `json:"ccPHIDs,omitempty"`
}

type createRevisionRequest struct {
	DiffID int            `json:"diffid"`
	Fields revisionFields `json:"fields,omitempty"`
}

type differentialRevision struct {
	RevisionID int        `json:"revisionid,omitempty"`
	URI        string     `json:"uri,omitempty"`
	Title      string     `json:"title,omitempty"`
	Branch     string     `json:"branch,omitempty"`
	Hashes     [][]string `json:"hashes,omitempty"`
}

type createRevisionResponse struct {
	Error        string               `json:"error,omitempty"`
	ErrorMessage string               `json:"errorMessage,omitempty"`
	Response     differentialRevision `json:"response,omitempty"`
}

func (arc Arcanist) createDifferentialRevision(repo repository.Repo, revision string, diffID int, req request.Request) (*differentialRevision, error) {
	// If the description is multiple lines, then treat the first as the title.
	fields := revisionFields{Title: strings.Split(req.Description, "\n")[0]}
	// Truncate the title if it is too long.
	if len(fields.Title) > differentialTitleLengthLimit {
		truncatedLimit := differentialTitleLengthLimit - 4
		fields.Title = fields.Title[0:truncatedLimit] + "..."
	}
	// If we modified the title from the description, then put the full description in the summary.
	if fields.Title != req.Description {
		fields.Summary = req.Description
	}
	for _, reviewer := range req.Reviewers {
		user, err := queryUser(reviewer)
		if err != nil {
			log.Print(err)
		} else if user != nil {
			fields.Reviewers = append(fields.Reviewers, user.PHID)
		}
	}
	if req.Requester != "" {
		user, err := queryUser(req.Requester)
		if err != nil {
			log.Print(err)
		} else if user != nil {
			fields.CCs = append(fields.CCs, user.PHID)
		}
	}
	createRequest := createRevisionRequest{diffID, fields}
	var createResponse createRevisionResponse
	runArcCommandOrDie("differential.createrevision", createRequest, &createResponse)
	if createResponse.Error != "" {
		return nil, fmt.Errorf("Failed to create the differential revision: %s", createResponse.ErrorMessage)
	}
	return &createResponse.Response, nil
}

type differentialUpdateRevisionRequest struct {
	ID     string `json:"id"`
	DiffID string `json:"diffid"`
	// Fields is a map of new values for the fields of the revision object. Any fields
	// that do not have corresponding entries are left unchanged.
	Fields map[string]interface{} `json:"fields,omitempty"`
}

type differentialUpdateRevisionResponse struct {
	Error        string `json:"error,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
}

func (differentialReview DifferentialReview) isClosed() bool {
	return differentialReview.Status == differentialClosedStatus || differentialReview.Status == differentialAbandonedStatus
}

type differentialCloseRequest struct {
	ID int `json:"revisionID"`
}

type differentialCloseResponse struct {
	Error        string `json:"error,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
}

func (differentialReview DifferentialReview) close() {
	reviewID, err := strconv.Atoi(differentialReview.ID)
	if err != nil {
		log.Panic(err)
	}
	closeRequest := differentialCloseRequest{reviewID}
	var closeResponse differentialCloseResponse
	runArcCommandOrDie("differential.close", closeRequest, &closeResponse)
	if closeResponse.Error != "" {
		// This might happen if someone merged in a review that wasn't accepted yet, or if the review is not owned by the robot account.
		log.Println(closeResponse.ErrorMessage)
	}
}

func findCommitForDiff(diffIDString string) string {
	diffID, err := strconv.Atoi(diffIDString)
	if err != nil {
		return ""
	}
	diff, err := readDiff(diffID)
	if err != nil {
		return ""
	}

	return diff.findLastCommit()
}

// createCommentRequest models the request format for
// Phabricator's differential.createcomment API method.
type createCommentRequest struct {
	RevisionID    string `json:"revision_id,omitempty"`
	Message       string `json:"content,omitempty"`
	Action        string `json:"action,omitempty"`
	AttachInlines bool   `json:"attach_inlines,omitempty"`
}

// createInlineRequest models the request format for
// Phabricator's differential.createinline API method.
type createInlineRequest struct {
	RevisionID string `json:"revisionID,omitempty"`
	DiffID     string `json:"diffID,omitempty"`
	FilePath   string `json:"filePath,omitempty"`
	LineNumber uint32 `json:"lineNumber,omitempty"`
	Content    string `json:"content,omitempty"`
	IsNewFile  uint32 `json:"isNewFile"`
}

// createInlineResponse models the response format for
// Phabricator's differential.createinline API method.
type createInlineResponse struct {
	Error        string `json:"error,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
}

// createCommentResponse models the response format for
// Phabricator's differential.createcomment API method.
type createCommentResponse struct {
	Error        string `json:"error,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
}

func overlapsAny(c comment.Comment, existingComments []comment.Comment) bool {
	for _, existing := range existingComments {
		if review_utils.Overlaps(c, existing) {
			return true
		}
	}
	return false
}

func (differentialReview DifferentialReview) buildCommentRequestsForThread(existingComments []comment.Comment, commentThread review.CommentThread, diffID, path string, lineNumber uint32) []createInlineRequest {
	var requests []createInlineRequest
	if !overlapsAny(commentThread.Comment, existingComments) {
		content := review_utils.QuoteDescription(commentThread.Comment)
		request := createInlineRequest{
			RevisionID: differentialReview.ID,
			DiffID:     diffID,
			FilePath:   path,
			LineNumber: lineNumber,
			// IsNewFile indicates if the comment is on the left-hand side (0) or the right-hand side (1).
			// We always post comments to the right-hand side.
			IsNewFile: 1,
			Content:   content,
		}
		requests = append(requests, request)
	}
	for _, child := range commentThread.Children {
		requests = append(requests, differentialReview.buildCommentRequestsForThread(existingComments, child, diffID, path, lineNumber)...)
	}
	return requests
}

func (differentialReview DifferentialReview) buildCommentRequests(commentThreads []review.CommentThread, existingComments []comment.Comment, commitToDiffMap map[string]string) ([]createInlineRequest, []createCommentRequest) {
	var inlineRequests []createInlineRequest
	var commentRequests []createCommentRequest

	for _, c := range commentThreads {
		if c.Comment.Location != nil && c.Comment.Location.Path != "" {
			// TODO(ojarjur): Also mirror whole-review comments.
			var lineNumber uint32 = 1
			if c.Comment.Location.Range != nil {
				lineNumber = c.Comment.Location.Range.StartLine
			}
			diffID := commitToDiffMap[c.Comment.Location.Commit]
			if diffID != "" {
				inlineRequests = append(inlineRequests, differentialReview.buildCommentRequestsForThread(existingComments, c, diffID, c.Comment.Location.Path, lineNumber)...)
			}
		}
	}
	if len(inlineRequests) > 0 {
		request := createCommentRequest{
			RevisionID:    differentialReview.ID,
			Action:        "comment",
			AttachInlines: true,
		}
		commentRequests = append(commentRequests, request)
	}
	return inlineRequests, commentRequests
}

type differentialUnitDiffProperty struct {
	Name   string `json:"name"`
	Link   string `json:"link"`
	Result string `json:"result"`
}

func translateReportStatusToDifferentialUnitResult(status string) string {
	if status == "success" {
		return "pass"
	} else if status == "failure" {
		return "fail"
	} else {
		// TODO(ojarjur): This is less than ideal. Phabricator no longer has a concept
		// of pending unit tests, so the closest match we have is that the tests have been
		// skipped.
		return "skip"
	}
}

type LintDiffProperty struct {
	Code        string `json:"code,omitempty"`
	Severity    string `json:"severity,omitempty"`
	Path        string `json:"path,omitempty"`
	Line        int    `json:"line,omitempty"`
	Description string `json:"description,omitempty"`
}

func (arc Arcanist) mirrorStatusesForEachCommit(r review.Review, commitToDiffIDMap map[string]int) {
	for commitHash, diffID := range commitToDiffIDMap {
		ciNotes := r.Repo.GetNotes(ci.Ref, commitHash)
		ciReports := ci.ParseAllValid(ciNotes)
		latestCIReport, err := ci.GetLatestCIReport(ciReports)
		if err != nil {
			log.Println("Failed to load the continuous integration reports: " + err.Error())
		} else if latestCIReport != nil {
			arc.reportUnitResults(diffID, *latestCIReport)
		}

		analysesNotes := r.Repo.GetNotes(analyses.Ref, commitHash)
		analysesReports := analyses.ParseAllValid(analysesNotes)
		latestAnalysesReport, err := analyses.GetLatestAnalysesReport(analysesReports)
		if err != nil {
			log.Println("Failed to load the static analysis reports: " + err.Error())
		} else if latestAnalysesReport != nil {
			lintResults, err := latestAnalysesReport.GetLintReportResult()
			if err != nil {
				log.Println("Failed to load the static analysis reports: " + err.Error())
			} else {
				arc.reportLintResults(diffID, lintResults)
			}
		}
	}
}

func (arc Arcanist) mirrorCommentsIntoReview(repo repository.Repo, differentialReview DifferentialReview, r review.Review) {
	commitToDiffMap := make(map[string]string)
	commitToDiffIDMap := make(map[string]int)
	for _, diffIDString := range differentialReview.Diffs {
		lastCommit := findCommitForDiff(diffIDString)
		commitToDiffMap[lastCommit] = diffIDString
		diffID, err := strconv.Atoi(diffIDString)
		if err == nil {
			commitToDiffIDMap[lastCommit] = diffID
		}
	}
	arc.mirrorStatusesForEachCommit(r, commitToDiffIDMap)

	existingComments := differentialReview.LoadComments()
	inlineRequests, commentRequests := differentialReview.buildCommentRequests(r.Comments, existingComments, commitToDiffMap)
	for _, request := range inlineRequests {
		var response createInlineResponse
		runArcCommandOrDie("differential.createinline", request, &response)
		if response.Error != "" {
			log.Println(response.ErrorMessage)
		}
	}
	for _, request := range commentRequests {
		var response createCommentResponse
		runArcCommandOrDie("differential.createcomment", request, &response)
		if response.Error != "" {
			log.Println(response.ErrorMessage)
		}
	}
}

func generateUnitDiffProperty(report ci.Report) (string, error) {
	if report.URL == "" {
		return "", nil
	}
	unitDiffProperty := differentialUnitDiffProperty{
		Name:   report.Agent,
		Link:   report.URL,
		Result: translateReportStatusToDifferentialUnitResult(report.Status),
	}
	// Note that although the unit tests property is a JSON object, Phabricator
	// expects there to be a list of such objects for any given diff. Therefore
	// we wrap the object in a list before marshaling it to send to the server.
	// TODO(ojarjur): We should take advantage of the fact that this is a list,
	// and include the latest CI report for each agent. That would allow us to
	// display results from multiple test runners in a code review.
	propertyBytes, err := json.Marshal([]differentialUnitDiffProperty{unitDiffProperty})
	if err != nil {
		return "", err
	}
	return string(propertyBytes), nil
}

func (arc Arcanist) reportUnitResults(diffID int, unitReport ci.Report) {
	log.Printf("The latest unit report for diff %d is %s ", diffID, unitReport)
	diffProperty, err := generateUnitDiffProperty(unitReport)
	if err == nil && diffProperty != "" {
		err = arc.setDiffProperty(diffID, unitDiffPropertyName, diffProperty)
	}
	if err != nil {
		log.Panic(err.Error())
	}
}

func generateLintDiffProperty(lintResults []analyses.AnalyzeResponse) (string, error) {
	var lintDiffProperties []LintDiffProperty
	for _, analyzeResponse := range lintResults {
		for _, note := range analyzeResponse.Notes {
			if note.Location != nil && note.Location.Range != nil {
				lintProperty := LintDiffProperty{
					Code: note.Category,
					// TODO(ojarjur): Don't just treat everything as a warning.
					Severity:    "warning",
					Path:        note.Location.Path,
					Line:        note.Location.Range.StartLine,
					Description: note.Description,
				}
				lintDiffProperties = append(lintDiffProperties, lintProperty)
			}
		}
	}
	if lintDiffProperties == nil {
		return "", nil
	}
	propertyBytes, err := json.Marshal(lintDiffProperties)
	return string(propertyBytes), err
}

func (arc Arcanist) reportLintResults(diffID int, lintResults []analyses.AnalyzeResponse) {
	log.Printf("The latest lint report for diff %d is %s ", diffID, lintResults)
	diffProperty, err := generateLintDiffProperty(lintResults)
	if err == nil && diffProperty != "" {
		err = arc.setDiffProperty(diffID, lintDiffPropertyName, diffProperty)
	}
	if err != nil {
		log.Panic(err.Error())
	}
}

// updateReviewDiffs updates the status of a differential review so that it matches the state of the repo.
//
// This consists of making sure the latest commit pushed to the review ref has a corresponding
// diff in the differential review.
func (arc Arcanist) updateReviewDiffs(repo repository.Repo, differentialReview DifferentialReview, headCommit string, req request.Request, r review.Review) {
	if differentialReview.isClosed() {
		return
	}

	headRevision := headCommit
	mergeBase, err := repo.MergeBase(req.TargetRef, headRevision)
	if err != nil {
		log.Panic(err)
	}
	for _, hashPair := range differentialReview.Hashes {
		if len(hashPair) == 2 && hashPair[0] == commitHashType && hashPair[1] == headCommit {
			// The review already has the hash of the HEAD commit, so we have nothing to do beyond mirroring comments
			// and build status if applicable
			arc.mirrorCommentsIntoReview(repo, differentialReview, r)
			return
		}
	}

	diff, err := arc.createDifferentialDiff(repo, mergeBase, headRevision, req, differentialReview.Diffs)
	if err != nil {
		log.Panic(err)
	}
	if diff == nil {
		// This means that phabricator silently refused to create the diff. Just move on.
		return
	}

	updateRequest := differentialUpdateRevisionRequest{ID: differentialReview.ID, DiffID: strconv.Itoa(diff.ID)}
	var updateResponse differentialUpdateRevisionResponse
	runArcCommandOrDie("differential.updaterevision", updateRequest, &updateResponse)
	if updateResponse.Error != "" {
		log.Panic(updateResponse.ErrorMessage)
	}
}

// EnsureRequestExists runs the "arcanist" command-line tool to create a Differential diff for the given request, if one does not already exist.
func (arc Arcanist) EnsureRequestExists(repo repository.Repo, review review.Review) {
	revision := review.Revision
	req := review.Request

	// If this revision has been previously closed shortcut all processing
	if closedRevisionsMap[revision] {
		return
	}
	existingReviews := arc.listDifferentialReviewsOrDie(revision)
	if review.Submitted {
		// The change has already been merged in, so we should simply close any open reviews.
		for _, differentialReview := range existingReviews {
			if !differentialReview.isClosed() {
				differentialReview.close()
			}
		}
		closedRevisionsMap[revision] = true
		return
	}

	base, err := review.GetBaseCommit()
	if err != nil {
		// There are lots of reasons that we might not be able to compute a base commit,
		// (e.g. the revision already being merged in, or being dropped and garbage collected),
		// but they all indicate that the review request is no longer valid.
		log.Printf("Ignoring review request '%v', because we could not compute a base commit", req)
		return
	}

	head, err := review.GetHeadCommit()
	if err != nil {
		// The given review ref has been deleted (or never existed), but the change wasn't merged.
		// TODO(ojarjur): We should mark the existing reviews as abandoned.
		log.Printf("Ignoring review because the review ref '%s' does not exist", req.ReviewRef)
		return
	}

	if len(existingReviews) > 0 {
		// The change is still pending, but we already have existing reviews, so we should just update those.
		for _, existing := range existingReviews {
			arc.updateReviewDiffs(repo, existing, head, req, review)
		}
		return
	}

	diff, err := arc.createDifferentialDiff(repo, base, revision, req, []string{})
	if err != nil {
		log.Panic(err)
	}
	if diff == nil {
		// The revision is already merged in, ignore it.
		return
	}
	rev, err := arc.createDifferentialRevision(repo, revision, diff.ID, req)
	if err != nil {
		log.Panic(err)
	}
	log.Printf("Created diff %v and revision %v for the review of %s", diff, rev, revision)

	// If the review already contains multiple commits by the time we mirror it, then
	// we need to ensure that at least the first and last ones are added.
	existingReviews = arc.listDifferentialReviewsOrDie(revision)
	for _, existing := range existingReviews {
		arc.updateReviewDiffs(repo, existing, head, req, review)
	}
}

// lookSoonRequest specifies a list of callsigns (repo identifier) for repos that have recently changed.
type lookSoonRequest struct {
	Callsigns []string `json:"callsigns,omitempty"`
}

// Refresh advises the review tool that the code being reviewed has changed, and to reload it.
//
// This corresponds to calling the diffusion.looksoon API.
func (arc Arcanist) Refresh(repo repository.Repo) {
	// We cannot determine the repo's callsign (the identifier Phabricator uses for the repo)
	// in all cases, but we can figure it out in the case that the mirror runs on the same
	// directories that Phabricator is using. In that scenario, the repo directories default
	// to being named "/var/repo/<CALLSIGN>", so if the repo path starts with that prefix then
	// we can try to strip out that prefix and use the rest as a callsign.
	if strings.HasPrefix(repo.GetPath(), defaultRepoDirPrefix) {
		possibleCallsign := strings.TrimPrefix(repo.GetPath(), defaultRepoDirPrefix)
		request := lookSoonRequest{Callsigns: []string{possibleCallsign}}
		response := make(map[string]interface{})
		runArcCommandOrDie("diffusion.looksoon", request, &response)
	}
}
