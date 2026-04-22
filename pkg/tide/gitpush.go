/*
Copyright 2024 The Kubernetes Authors.

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

package tide

import (
	"fmt"

	"github.com/sirupsen/logrus"
	prowapi "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/config"
	git "sigs.k8s.io/prow/pkg/git/v2"
	"sigs.k8s.io/prow/pkg/tide/blockers"

	gitTypes "sigs.k8s.io/prow/pkg/git/types"
)

// RemoteURLFunc returns the authenticated remote URL for a given (org, repo)
// pair. The URL must embed any credentials required to push (e.g. a personal
// access token in the userinfo component: "https://token@host/org/repo.git").
type RemoteURLFunc func(org, repo string) (string, error)

// GitPushProvider wraps any inner provider and replaces the merge step with a
// plain `git push` (fast-forward only) instead of calling the platform's merge
// API. All other provider methods are delegated to the inner provider unchanged.
//
// Because commits are pushed directly — without the platform creating a merge
// commit — both the commit content and the resulting SHA are identical on every
// platform that receives the same push. This is the key property needed by the
// pairProvider (Part 2) for cross-platform SHA consistency.
//
// For batch merges, each PR in the batch must already be linearly rebased on
// top of the preceding PR's HEAD (or the base branch tip for the first PR).
// Tide's pickNewBatch will only select fast-forward-compatible PRs when
// prMergeMethod returns MergeGitPush.
type GitPushProvider struct {
	inner     provider
	gc        git.ClientFactory
	remoteURL RemoteURLFunc
	logger    *logrus.Entry
}

// NewGitPushProvider returns a GitPushProvider that wraps inner and uses gc to
// clone repositories and remoteURL to obtain the authenticated push destination.
func NewGitPushProvider(
	inner provider,
	gc git.ClientFactory,
	remoteURL RemoteURLFunc,
	logger *logrus.Entry,
) *GitPushProvider {
	return &GitPushProvider{
		inner:     inner,
		gc:        gc,
		remoteURL: remoteURL,
		logger:    logger,
	}
}

// prMergeMethod always returns MergeGitPush so that Tide's batch-selection
// logic (pickNewBatch) uses --ff-only for local compatibility checks.
func (p *GitPushProvider) prMergeMethod(_ *CodeReviewCommon) *gitTypes.PullRequestMergeType {
	m := gitTypes.MergeGitPush
	return &m
}

// mergePRs merges each PR by fast-forward pushing its HEAD commit directly to
// the remote base branch. Merges stop at the first failure so that the remote
// branch is always in a consistent state.
//
// Each PR must be rebased on top of the base branch (or the previous PR's HEAD
// when processing a batch). The remote will reject any non-fast-forward push,
// which surfaces as an error here.
func (p *GitPushProvider) mergePRs(sp subpool, prs []CodeReviewCommon, _ *threadSafePRSet) ([]CodeReviewCommon, error) {
	if len(prs) == 0 {
		return nil, nil
	}

	remoteURL, err := p.remoteURL(sp.org, sp.repo)
	if err != nil {
		return nil, fmt.Errorf("gitpush: build remote URL for %s/%s: %w", sp.org, sp.repo, err)
	}

	r, err := p.gc.ClientFor(sp.org, sp.repo)
	if err != nil {
		return nil, fmt.Errorf("gitpush: git client for %s/%s: %w", sp.org, sp.repo, err)
	}
	defer r.Clean()

	if err := r.Config("user.name", "prow"); err != nil {
		return nil, fmt.Errorf("gitpush: git config user.name: %w", err)
	}
	if err := r.Config("user.email", "prow@localhost"); err != nil {
		return nil, fmt.Errorf("gitpush: git config user.email: %w", err)
	}
	// Ignore gpgsign errors — the git version may not support it.
	_ = r.Config("commit.gpgsign", "false")

	var merged []CodeReviewCommon
	for _, pr := range prs {
		log := p.logger.WithFields(pr.logFields())

		// Fetch the PR HEAD by its standard pull-request refspec so the SHA is
		// available locally. This also verifies reachability from the remote.
		prRefspec := fmt.Sprintf("pull/%d/head", pr.Number)
		if err := r.FetchRef(prRefspec); err != nil {
			log.WithError(err).Warn("gitpush: failed to fetch PR head, stopping merge batch.")
			break
		}

		// Sanity-check: the fetched HEAD must match what Tide recorded.
		fetchedSHA, err := r.ShowRef("FETCH_HEAD")
		if err != nil {
			log.WithError(err).Warn("gitpush: failed to resolve FETCH_HEAD, stopping merge batch.")
			break
		}
		if fetchedSHA != pr.HeadRefOID {
			log.Warnf("gitpush: fetched SHA %q does not match expected %q, stopping merge batch.", fetchedSHA, pr.HeadRefOID)
			break
		}

		// Push the PR HEAD SHA as a fast-forward update to the base branch.
		// The remote rejects this if the PR is not rebased on the current tip.
		if err := r.PushSHAToBranch(remoteURL, pr.HeadRefOID, sp.branch); err != nil {
			log.WithError(err).Warn("gitpush: fast-forward push rejected, stopping merge batch.")
			break
		}

		log.Info("gitpush: merged via git push.")
		merged = append(merged, pr)
	}

	return merged, nil
}

// --------------------------------------------------------------------------
// All remaining provider methods are pure delegation to the inner provider.
// --------------------------------------------------------------------------

func (p *GitPushProvider) Query() (map[string]CodeReviewCommon, error) {
	return p.inner.Query()
}

func (p *GitPushProvider) blockers() (blockers.Blockers, error) {
	return p.inner.blockers()
}

func (p *GitPushProvider) isAllowedToMerge(crc *CodeReviewCommon) (string, error) {
	return p.inner.isAllowedToMerge(crc)
}

func (p *GitPushProvider) GetRef(org, repo, ref string) (string, error) {
	return p.inner.GetRef(org, repo, ref)
}

func (p *GitPushProvider) headContexts(pr *CodeReviewCommon) ([]Context, error) {
	return p.inner.headContexts(pr)
}

func (p *GitPushProvider) GetTideContextPolicy(org, repo, branch string, baseSHAGetter config.RefGetter, pr *CodeReviewCommon) (contextChecker, error) {
	return p.inner.GetTideContextPolicy(org, repo, branch, baseSHAGetter, pr)
}

func (p *GitPushProvider) GetPresubmits(identifier, baseBranch string, baseSHAGetter config.RefGetter, headSHAGetters ...config.RefGetter) ([]config.Presubmit, error) {
	return p.inner.GetPresubmits(identifier, baseBranch, baseSHAGetter, headSHAGetters...)
}

func (p *GitPushProvider) GetChangedFiles(org, repo string, number int) ([]string, error) {
	return p.inner.GetChangedFiles(org, repo, number)
}

func (p *GitPushProvider) refsForJob(sp subpool, prs []CodeReviewCommon) (prowapi.Refs, error) {
	return p.inner.refsForJob(sp, prs)
}

func (p *GitPushProvider) labelsAndAnnotations(instance string, jobLabels, jobAnnotations map[string]string, changes ...CodeReviewCommon) (labels, annotations map[string]string) {
	return p.inner.labelsAndAnnotations(instance, jobLabels, jobAnnotations, changes...)
}

func (p *GitPushProvider) jobIsRequiredByTide(ps *config.Presubmit, pr *CodeReviewCommon) bool {
	return p.inner.jobIsRequiredByTide(ps, pr)
}
