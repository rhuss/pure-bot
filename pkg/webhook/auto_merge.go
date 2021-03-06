// Copyright © 2017 Syndesis Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package webhook

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/go-github/github"
	"github.com/pkg/errors"
	"github.com/syndesisio/pure-bot/pkg/config"
	"go.uber.org/multierr"
	"go.uber.org/zap"
)

const (
	labeledEvent                = "labeled"
	statusEventSuccessState     = "success"
	checkEventSuccessConclusion = "success"
)

type autoMerger struct{}

func (h *autoMerger) EventTypesHandled() []string {
	return []string{"pull_request", "status", "pull_request_review"}
}

func (h *autoMerger) HandleEvent(eventObject interface{}, gh *github.Client, config config.RepoConfig, logger *zap.Logger) error {

	approvedLabel := config.Labels.Approved
	if approvedLabel == "" {
		return nil
	}

	switch event := eventObject.(type) {
	case *github.PullRequestEvent:
		return h.handlePullRequestEvent(event, gh, config, logger)
	case *github.StatusEvent:
		return h.handleStatusEvent(event, gh, config, logger)
	case *github.PullRequestReviewEvent:
		return h.handlePullRequestReviewEvent(event, gh, config, logger)
	default:
		return nil
	}
}

func (h *autoMerger) handlePullRequestReviewEvent(event *github.PullRequestReviewEvent, gh *github.Client, config config.RepoConfig, logger *zap.Logger) error {
	if strings.ToLower(event.Review.GetState()) != approvedReviewState {
		logger.Debug("skipping PullRequestReview event as its not in approved state", zap.String("state", event.Review.GetState()), zap.Int("pr", event.PullRequest.GetNumber()))
		return nil
	}

	return h.mergePRFromPullRequestEvent(event.Installation.GetID(), event.Repo, event.PullRequest, gh, config, logger)
}

func (h *autoMerger) handlePullRequestEvent(event *github.PullRequestEvent, gh *github.Client, config config.RepoConfig, logger *zap.Logger) error {

	if strings.ToLower(event.GetAction()) != labeledEvent {
		logger.Debug("skipping PullRequest event as it is not a label event", zap.String("action", event.GetAction()), zap.Int("pr", event.PullRequest.GetNumber()))
		return nil
	}

	return h.mergePRFromPullRequestEvent(event.Installation.GetID(), event.Repo, event.PullRequest, gh, config, logger)
}

func (h *autoMerger) handleStatusEvent(event *github.StatusEvent, gh *github.Client, config config.RepoConfig, logger *zap.Logger) error {

	if strings.ToLower(event.GetState()) != statusEventSuccessState {
		logger.Debug("skipping status event as it dosn't report success: ", zap.String("state", event.GetState()))
		return nil
	}

	commitSHA := event.GetSHA()
	query := fmt.Sprintf("type:pr state:open repo:%s %s", event.Repo.GetFullName(), commitSHA)
	searchResult, _, err := gh.Search.Issues(context.Background(), query, nil)
	if err != nil {
		return errors.Wrap(err, "failed to search for open issues")
	}
	var multiErr error
	for _, issue := range searchResult.Issues {
		if issue.PullRequestLinks == nil {
			continue
		}

		pr, _, err := gh.PullRequests.Get(context.Background(), event.Repo.Owner.GetLogin(), event.Repo.GetName(), issue.GetNumber())
		if err != nil {
			multiErr = multierr.Combine(multiErr, err)
			continue
		}

		err = mergePR(&issue, pr, event.Repo.Owner.GetLogin(), event.Repo.GetName(), gh, commitSHA, config, logger)
		if err != nil {
			multiErr = multierr.Combine(multiErr, err)
			continue
		}
	}

	return multiErr
}

func (h *autoMerger) mergePRFromPullRequestEvent(installationID int64, repo *github.Repository, pullRequest *github.PullRequest, gh *github.Client, config config.RepoConfig, logger *zap.Logger) error {
	issue, _, err := gh.Issues.Get(context.Background(), repo.Owner.GetLogin(), repo.GetName(), pullRequest.GetNumber())
	if err != nil {
		return errors.Wrapf(err, "failed to get pull request %s", pullRequest.GetHTMLURL())
	}

	return mergePR(issue, pullRequest, repo.Owner.GetLogin(), repo.GetName(), gh, "", config, logger)
}

func mergePR(issue *github.Issue, pr *github.PullRequest, owner, repository string, gh *github.Client, commitSHA string, config config.RepoConfig, logger *zap.Logger) error {
	if !containsLabel(issue.Labels, config.Labels.Approved) {
		return nil
	}

	if commitSHA != "" && pr.Head.GetSHA() != commitSHA {
		logger.Debug("Commit SHA is unequal PR Head SHA", zap.String("commitSHA", commitSHA), zap.String("prHeadSha", pr.Head.GetSHA()))
		return nil
	}
	commitSHA = pr.Head.GetSHA()

	statuses, _, err := gh.Repositories.GetCombinedStatus(context.Background(), owner, repository, commitSHA, nil)
	if err != nil {
		return errors.Wrapf(err, "failed to get statuses of pull request %s", issue.GetHTMLURL())
	}

	prStatusMap := make(map[string]bool, len(statuses.Statuses))
	for _, status := range statuses.Statuses {
		logger.Debug("found PR status", zap.String("context", status.GetContext()), zap.String("state", status.GetState()))
		prStatusMap[status.GetContext()] = status.GetState() == statusEventSuccessState
	}

	prChecks, _, err := gh.Checks.ListCheckRunsForRef(context.Background(), owner, repository, commitSHA, nil)
	if err != nil {
		return errors.Wrapf(err, "failed to retrieve all check for pull request %s", issue.GetHTMLURL())
	}

	for _, check := range prChecks.CheckRuns {

		logger.Debug("found PR check", zap.String("name", *check.Name), zap.Any("conclusion", check.Conclusion), zap.String("ref", commitSHA))
		prStatusMap[*check.Name] = check.Conclusion != nil && *check.Conclusion == checkEventSuccessConclusion

	}

	requiredContexts, _, err := gh.Repositories.ListRequiredStatusChecksContexts(context.Background(), owner, repository, pr.Base.GetRef())
	if err != nil {
		if errResp, ok := err.(*github.ErrorResponse); !ok || errResp.Response.StatusCode != http.StatusNotFound {
			return errors.Wrapf(err, "failed to get target branch (%s) protection for pull request %s", pr.Base.GetRef(), issue.GetHTMLURL())
		}
	}

	if len(requiredContexts) == 0 {
		for _, contextStatus := range prStatusMap {
			if !contextStatus {
				return nil
			}
		}
	} else {
		for _, requiredContext := range requiredContexts {
			if success, present := prStatusMap[requiredContext]; !present || !success {
				logger.Debug("don't merging because status/check failed", zap.String("context", requiredContext), zap.Bool("present", present), zap.Bool("success", success))
				return nil
			}
		}
	}

	_, _, err = gh.PullRequests.Merge(context.Background(), owner, repository, issue.GetNumber(), "", &github.PullRequestOptions{
		SHA: commitSHA,
	})
	if err != nil {
		return errors.Wrapf(err, "failed to merge pull request %s", issue.GetHTMLURL())
	}
	logger.Debug("Successfully merged " + owner + "/" + repository + ": " + strconv.Itoa(issue.GetNumber()))
	return nil
}
