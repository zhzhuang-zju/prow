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

// PairProvider coordinates two providers — first (GitHub) and second (GitCode)
// — so that merges are applied to both platforms in order while preserving
// identical commit SHAs.
//
// Design rationale
// ================
// Users may open PRs on either GitHub or GitCode.  Regardless of which
// platform the PR originates from, Tide must:
//   1. Verify that both platform's base branches are at the same commit.
//   2. Push the PR's commit(s) to GitHub first, then to GitCode.
//
// This guarantees that both repos always contain exactly the same history with
// exactly the same commit SHAs — no platform-side merge commits are created.
//
// The git clone is always initiated from the GitHub remote (since both repos
// carry identical history).  For GitCode PRs the PR HEAD is fetched from
// GitCode's remote URL using the standard `pull/<N>/head` refspec.
//
// Collision note
// ==============
// If a GitHub PR and a GitCode PR share the same PR number for the same
// org/repo at the same time, the GitHub entry takes precedence in the Query
// result map.  This is a known V1 limitation; in practice users open PRs on
// one platform at a time.

import (
	"fmt"
	"sync"

	"github.com/sirupsen/logrus"
	prowapi "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/config"
	git "sigs.k8s.io/prow/pkg/git/v2"
	gitTypes "sigs.k8s.io/prow/pkg/git/types"
	"sigs.k8s.io/prow/pkg/tide/blockers"
)

const (
	platformGitHub  = "github"
	platformGitCode = "gitcode"
)

// Enforce interface implementation at compile time.
var _ provider = (*PairProvider)(nil)

// PairProvider implements the provider interface for a mirrored
// GitHub ↔ GitCode setup.  first must be the GitHub provider; second must be
// the GitCode provider.
type PairProvider struct {
	first  provider // GitHub
	second provider // GitCode

	// gc clones repos from GitHub (both remotes share identical history).
	gc git.ClientFactory

	// firstRemoteURL returns the authenticated GitHub push URL for org/repo.
	firstRemoteURL RemoteURLFunc
	// secondRemoteURL returns the authenticated GitCode fetch+push URL for org/repo.
	secondRemoteURL RemoteURLFunc

	// prPlatform records which platform each PR originates from, populated by
	// Query.  Key: prKey(crc), Value: platformGitHub or platformGitCode.
	mu         sync.RWMutex
	prPlatform map[string]string

	logger *logrus.Entry
}

// NewPairProvider creates a PairProvider.
//   - first must be the GitHubProvider (or a GitPushProvider wrapping it).
//   - second must be the GitCodeProvider (or a GitPushProvider wrapping it).
//   - gc is the git.ClientFactory used for cloning (configured against GitHub).
//   - firstRemoteURL / secondRemoteURL return authenticated push URLs.
func NewPairProvider(
	first, second provider,
	gc git.ClientFactory,
	firstRemoteURL, secondRemoteURL RemoteURLFunc,
	logger *logrus.Entry,
) *PairProvider {
	return &PairProvider{
		first:           first,
		second:          second,
		gc:              gc,
		firstRemoteURL:  firstRemoteURL,
		secondRemoteURL: secondRemoteURL,
		prPlatform:      make(map[string]string),
		logger:          logger,
	}
}

// --------------------------------------------------------------------------
// Query
// --------------------------------------------------------------------------

// Query returns the union of PRs from both platforms.
// GitHub entries take precedence on key collision (same org/repo and PR number
// present on both platforms simultaneously — a known V1 limitation).
func (p *PairProvider) Query() (map[string]CodeReviewCommon, error) {
	gcPRs, gcErr := p.second.Query()
	ghPRs, ghErr := p.first.Query()

	result := make(map[string]CodeReviewCommon)

	p.mu.Lock()
	defer p.mu.Unlock()
	p.prPlatform = make(map[string]string)

	// GitCode entries first so GitHub can overwrite on collision.
	for k, v := range gcPRs {
		result[k] = v
		p.prPlatform[k] = platformGitCode
	}
	for k, v := range ghPRs {
		result[k] = v
		p.prPlatform[k] = platformGitHub
	}

	// Return partial results with a combined error if either side failed.
	var combinedErr error
	if gcErr != nil && ghErr != nil {
		combinedErr = fmt.Errorf("github query: %w; gitcode query: %v", ghErr, gcErr)
	} else if ghErr != nil {
		combinedErr = fmt.Errorf("github query: %w", ghErr)
	} else if gcErr != nil {
		combinedErr = fmt.Errorf("gitcode query: %w", gcErr)
	}
	return result, combinedErr
}

// --------------------------------------------------------------------------
// mergePRs — the core cross-platform merge operation
// --------------------------------------------------------------------------

// mergePRs verifies that both platforms are in sync, then for each PR:
//  1. Fetches the PR HEAD from its source platform.
//  2. Pushes the SHA to GitHub (first).
//  3. Pushes the same SHA to GitCode (second).
//
// Merges stop at the first failure to keep both remotes consistent.
func (p *PairProvider) mergePRs(sp subpool, prs []CodeReviewCommon, _ *threadSafePRSet) ([]CodeReviewCommon, error) {
	log := p.logger.WithFields(logrus.Fields{
		"org":    sp.org,
		"repo":   sp.repo,
		"branch": sp.branch,
	})

	// Verify both platforms share the same base-branch tip.
	firstRef, err := p.first.GetRef(sp.org, sp.repo, "heads/"+sp.branch)
	if err != nil {
		return nil, fmt.Errorf("pair: get GitHub ref for %s/%s heads/%s: %w", sp.org, sp.repo, sp.branch, err)
	}
	secondRef, err := p.second.GetRef(sp.org, sp.repo, "heads/"+sp.branch)
	if err != nil {
		return nil, fmt.Errorf("pair: get GitCode ref for %s/%s heads/%s: %w", sp.org, sp.repo, sp.branch, err)
	}
	if firstRef != secondRef {
		return nil, fmt.Errorf("pair: platforms have diverged — GitHub=%s GitCode=%s for %s/%s:%s; merge blocked until they are reconciled",
			firstRef, secondRef, sp.org, sp.repo, sp.branch)
	}

	firstURL, err := p.firstRemoteURL(sp.org, sp.repo)
	if err != nil {
		return nil, fmt.Errorf("pair: build GitHub remote URL for %s/%s: %w", sp.org, sp.repo, err)
	}
	secondURL, err := p.secondRemoteURL(sp.org, sp.repo)
	if err != nil {
		return nil, fmt.Errorf("pair: build GitCode remote URL for %s/%s: %w", sp.org, sp.repo, err)
	}

	r, err := p.gc.ClientFor(sp.org, sp.repo)
	if err != nil {
		return nil, fmt.Errorf("pair: git client for %s/%s: %w", sp.org, sp.repo, err)
	}
	defer r.Clean()

	if err := r.Config("user.name", "prow"); err != nil {
		return nil, fmt.Errorf("pair: git config user.name: %w", err)
	}
	if err := r.Config("user.email", "prow@localhost"); err != nil {
		return nil, fmt.Errorf("pair: git config user.email: %w", err)
	}
	_ = r.Config("commit.gpgsign", "false")

	var merged []CodeReviewCommon
	for _, pr := range prs {
		prLog := log.WithFields(pr.logFields())

		// Fetch the PR HEAD from its source platform.
		if err := p.fetchPRHead(r, pr, secondURL); err != nil {
			prLog.WithError(err).Warn("pair: failed to fetch PR head; stopping merge batch.")
			break
		}

		// Confirm the fetched SHA matches what Tide expects.
		fetchedSHA, err := r.ShowRef("FETCH_HEAD")
		if err != nil {
			prLog.WithError(err).Warn("pair: failed to resolve FETCH_HEAD; stopping merge batch.")
			break
		}
		if fetchedSHA != pr.HeadRefOID {
			prLog.Warnf("pair: fetched SHA %q does not match expected %q; stopping merge batch.", fetchedSHA, pr.HeadRefOID)
			break
		}

		// Push to GitHub first.
		if err := r.PushSHAToBranch(firstURL, pr.HeadRefOID, sp.branch); err != nil {
			prLog.WithError(err).Warn("pair: push to GitHub rejected; stopping merge batch.")
			break
		}
		prLog.Info("pair: pushed to GitHub.")

		// Push the identical SHA to GitCode.
		if err := r.PushSHAToBranch(secondURL, pr.HeadRefOID, sp.branch); err != nil {
			prLog.WithError(err).Warn("pair: push to GitCode rejected; stopping merge batch.")
			// GitHub has already received this commit. We break to avoid a
			// partial state but can't roll back the GitHub push.
			break
		}
		prLog.Info("pair: pushed to GitCode.")

		merged = append(merged, pr)
	}
	return merged, nil
}

// fetchPRHead fetches the HEAD commit of pr from its source platform.
// For GitHub PRs the standard `pull/<N>/head` refspec is used against the
// clone's origin (GitHub).  For GitCode PRs the same refspec is fetched
// from the GitCode remote URL so that no GitHub PR is required.
func (p *PairProvider) fetchPRHead(r git.RepoClient, pr CodeReviewCommon, gitcodeURL string) error {
	if pr.GitCode != nil {
		// Fetch from GitCode remote using the pull/<N>/head refspec.
		pullRefspec := fmt.Sprintf("pull/%d/head", pr.Number)
		return r.FetchFromRemote(
			git.RemoteResolver(func() (string, error) { return gitcodeURL, nil }),
			pullRefspec,
		)
	}
	// Default: GitHub PR — fetch from the configured origin.
	return r.FetchRef(fmt.Sprintf("pull/%d/head", pr.Number))
}

// --------------------------------------------------------------------------
// Routing helpers
// --------------------------------------------------------------------------

// platformForPR returns the platform for a given PR using the prPlatform map.
func (p *PairProvider) platformForPR(crc *CodeReviewCommon) string {
	if crc.GitCode != nil {
		return platformGitCode
	}
	if crc.GitHub != nil {
		return platformGitHub
	}
	// Fall back to the lookup table populated by Query.
	p.mu.RLock()
	defer p.mu.RUnlock()
	if plat, ok := p.prPlatform[prKey(crc)]; ok {
		return plat
	}
	return platformGitHub // default to GitHub
}

// --------------------------------------------------------------------------
// provider interface — routing implementations
// --------------------------------------------------------------------------

func (p *PairProvider) blockers() (blockers.Blockers, error) {
	// Blocker issues are a GitHub concept; delegate to GitHub provider.
	return p.first.blockers()
}

func (p *PairProvider) isAllowedToMerge(crc *CodeReviewCommon) (string, error) {
	if p.platformForPR(crc) == platformGitCode {
		return p.second.isAllowedToMerge(crc)
	}
	return p.first.isAllowedToMerge(crc)
}

// GetRef uses GitHub as the authoritative source of truth.
// Both platforms should always be at the same SHA; divergence is caught in mergePRs.
func (p *PairProvider) GetRef(org, repo, ref string) (string, error) {
	return p.first.GetRef(org, repo, ref)
}

func (p *PairProvider) headContexts(pr *CodeReviewCommon) ([]Context, error) {
	if p.platformForPR(pr) == platformGitCode {
		return p.second.headContexts(pr)
	}
	return p.first.headContexts(pr)
}

func (p *PairProvider) GetTideContextPolicy(org, repo, branch string, baseSHAGetter config.RefGetter, pr *CodeReviewCommon) (contextChecker, error) {
	if p.platformForPR(pr) == platformGitCode {
		return p.second.GetTideContextPolicy(org, repo, branch, baseSHAGetter, pr)
	}
	return p.first.GetTideContextPolicy(org, repo, branch, baseSHAGetter, pr)
}

// prMergeMethod always returns MergeGitPush — pairProvider always merges via git push.
func (p *PairProvider) prMergeMethod(_ *CodeReviewCommon) *gitTypes.PullRequestMergeType {
	m := gitTypes.MergeGitPush
	return &m
}

// GetPresubmits delegates to the GitHub provider since presubmit config is
// typically stored in the GitHub-side repository.
func (p *PairProvider) GetPresubmits(identifier, baseBranch string, baseSHAGetter config.RefGetter, headSHAGetters ...config.RefGetter) ([]config.Presubmit, error) {
	return p.first.GetPresubmits(identifier, baseBranch, baseSHAGetter, headSHAGetters...)
}

// GetChangedFiles routes to the platform that owns the PR.
// For the pairProvider the prPlatform map (populated during Query) is used to
// determine ownership when the CRC is not available in context.
func (p *PairProvider) GetChangedFiles(org, repo string, number int) ([]string, error) {
	// Build a minimal CRC key for platform lookup.
	key := fmt.Sprintf("%s/%s#%d", org, repo, number)
	p.mu.RLock()
	plat := p.prPlatform[key]
	p.mu.RUnlock()

	if plat == platformGitCode {
		return p.second.GetChangedFiles(org, repo, number)
	}
	return p.first.GetChangedFiles(org, repo, number)
}

func (p *PairProvider) refsForJob(sp subpool, prs []CodeReviewCommon) (prowapi.Refs, error) {
	// Route based on the first PR's platform; mixed batches are not expected
	// since Tide groups PRs by subpool (org/repo/branch) and both platforms
	// share the same repo coordinates.
	if len(prs) > 0 && p.platformForPR(&prs[0]) == platformGitCode {
		return p.second.refsForJob(sp, prs)
	}
	return p.first.refsForJob(sp, prs)
}

func (p *PairProvider) labelsAndAnnotations(instance string, jobLabels, jobAnnotations map[string]string, changes ...CodeReviewCommon) (labels, annotations map[string]string) {
	if len(changes) > 0 && p.platformForPR(&changes[0]) == platformGitCode {
		return p.second.labelsAndAnnotations(instance, jobLabels, jobAnnotations, changes...)
	}
	return p.first.labelsAndAnnotations(instance, jobLabels, jobAnnotations, changes...)
}

func (p *PairProvider) jobIsRequiredByTide(ps *config.Presubmit, pr *CodeReviewCommon) bool {
	if p.platformForPR(pr) == platformGitCode {
		return p.second.jobIsRequiredByTide(ps, pr)
	}
	return p.first.jobIsRequiredByTide(ps, pr)
}
