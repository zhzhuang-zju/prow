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
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	configflagutil "sigs.k8s.io/prow/pkg/flagutil/config"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	prowapi "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/config"
	"sigs.k8s.io/prow/pkg/git/types"
	gitv2 "sigs.k8s.io/prow/pkg/git/v2"
	"sigs.k8s.io/prow/pkg/gitcode"
	"sigs.k8s.io/prow/pkg/io"
	"sigs.k8s.io/prow/pkg/kube"
	"sigs.k8s.io/prow/pkg/moonraker"
	"sigs.k8s.io/prow/pkg/tide/blockers"
	"sigs.k8s.io/prow/pkg/tide/history"

	githubql "github.com/shurcooL/githubv4"
	"github.com/sirupsen/logrus"
)

// gitcodeContextChecker is a permissive no-op contextChecker for GitCode. Like
// Gerrit, GitCode merge-readiness is expressed through the platform's own
// submission rules, not through GitHub-style status contexts, so every context
// is treated as optional by Tide.
type gitcodeContextChecker struct{}

func (gcc *gitcodeContextChecker) IsOptional(string) bool         { return true }
func (gcc *gitcodeContextChecker) MissingRequiredContexts([]string) []string { return nil }

// NewGitCodeController creates a Tide Controller backed by a GitCode provider.
func NewGitCodeController(
	mgr manager,
	cfgAgent *config.Agent,
	gc gitv2.ClientFactory,
	maxRecordsPerPool int,
	opener io.Opener,
	historyURI,
	statusURI string,
	logger *logrus.Entry,
	configOptions configflagutil.ConfigOptions,
) (*Controller, error) {
	if logger == nil {
		logger = logrus.NewEntry(logrus.StandardLogger())
	}

	hist, err := history.New(maxRecordsPerPool, opener, historyURI)
	if err != nil {
		return nil, fmt.Errorf("error initializing history client from %q: %w", historyURI, err)
	}

	ctx := context.Background()
	statusUpdate := &statusUpdate{
		dontUpdateStatus: &threadSafePRSet{},
		newPoolPending:   make(chan bool),
	}

	var ircg config.InRepoConfigGetter
	if configOptions.MoonrakerAddress != "" {
		moonrakerClient, err := moonraker.NewClient(configOptions.MoonrakerAddress, cfgAgent)
		if err != nil {
			logrus.WithError(err).Fatal("Error getting Moonraker client.")
		}
		ircg = moonrakerClient
	} else {
		ircg, err = config.NewInRepoConfigCache(configOptions.InRepoConfigCacheSize, cfgAgent, gc)
		if err != nil {
			return nil, fmt.Errorf("failed creating inrepoconfig cache: %w", err)
		}
	}

	gitCodeProvider := newGitCodeProvider(logger, cfgAgent.Config, mgr.GetClient(), ircg)
	syncCtrl, err := newSyncController(ctx, logger, mgr, gitCodeProvider, cfgAgent.Config, gc, hist, false, statusUpdate)
	if err != nil {
		return nil, err
	}
	return &Controller{syncCtrl: syncCtrl}, nil
}

// Enforcing interface implementation check at compile time.
var _ provider = (*GitCodeProvider)(nil)

// GitCodeProvider implements the provider interface for GitCode, enabling the
// Tide sync controller to manage GitCode merge requests automatically.
type GitCodeProvider struct {
	cfg                config.Getter
	gc                 gitcode.Client
	pjclientset        ctrlruntimeclient.Client
	inRepoConfigGetter config.InRepoConfigGetter
	logger             *logrus.Entry
}

func newGitCodeProvider(
	logger *logrus.Entry,
	cfg config.Getter,
	pjclientset ctrlruntimeclient.Client,
	ircg config.InRepoConfigGetter,
) *GitCodeProvider {
	gitCodeCfg := cfg().Tide.GitCode
	var endpoint, token string
	if gitCodeCfg != nil {
		endpoint = gitCodeCfg.Endpoint
		if gitCodeCfg.TokenPath != "" {
			data, err := os.ReadFile(gitCodeCfg.TokenPath)
			if err != nil {
				logrus.WithError(err).Fatal("Error reading GitCode token file.")
			}
			token = strings.TrimSpace(string(data))
		}
	}

	return &GitCodeProvider{
		logger:             logger,
		cfg:                cfg,
		pjclientset:        pjclientset,
		gc:                 gitcode.NewClient(endpoint, token),
		inRepoConfigGetter: ircg,
	}
}

// Query returns all open merge requests from the configured GitCode org/repos.
func (p *GitCodeProvider) Query() (map[string]CodeReviewCommon, error) {
	gitCodeCfg := p.cfg().Tide.GitCode
	if gitCodeCfg == nil {
		return nil, nil
	}

	type mrFromRepo struct {
		org, repo string
		mrs       []gitcode.MergeRequest
	}
	resChan := make(chan mrFromRepo)
	errChan := make(chan error)

	var wg sync.WaitGroup
	for org, repos := range gitCodeCfg.Queries.AllRepos() {
		for _, repo := range repos {
			wg.Add(1)
			go func(org, repo string) {
				defer wg.Done()
				mrs, err := p.gc.ListOpenMergeRequests(org, repo)
				if err != nil {
					p.logger.WithFields(logrus.Fields{"org": org, "repo": repo}).
						WithError(err).Warn("Querying GitCode repo for open MRs.")
					errChan <- fmt.Errorf("querying %s/%s: %w", org, repo, err)
					return
				}
				resChan <- mrFromRepo{org: org, repo: repo, mrs: mrs}
			}(org, repo)
		}
	}

	go func() {
		wg.Wait()
		close(resChan)
		close(errChan)
	}()

	res := make(map[string]CodeReviewCommon)
	var errs []error
	for {
		select {
		case item, ok := <-resChan:
			if !ok {
				resChan = nil
			} else {
				for i := range item.mrs {
					crc := CodeReviewCommonFromGitCode(&item.mrs[i], item.org, item.repo)
					res[prKey(crc)] = *crc
				}
			}
		case err, ok := <-errChan:
			if !ok {
				errChan = nil
			} else {
				errs = append(errs, err)
			}
		}
		if resChan == nil && errChan == nil {
			break
		}
	}

	if len(errs) > 0 && len(res) == 0 {
		return nil, utilerrors.NewAggregate(errs)
	}
	return res, nil
}

func (p *GitCodeProvider) blockers() (blockers.Blockers, error) {
	// Blocker issues are a GitHub-specific feature; not supported for GitCode.
	return blockers.Blockers{}, nil
}

func (p *GitCodeProvider) isAllowedToMerge(crc *CodeReviewCommon) (string, error) {
	if crc.Mergeable == string(githubql.MergeableStateConflicting) {
		return "PR has a merge conflict.", nil
	}
	return "", nil
}

// GetRef returns the HEAD SHA of the given branch (ref should be of the form
// "heads/<branch>").
func (p *GitCodeProvider) GetRef(org, repo, ref string) (string, error) {
	branch := strings.TrimPrefix(ref, "heads/")
	return p.gc.GetBranchSHA(org, repo, branch)
}

// headContexts builds Tide contexts from ProwJobs for the given PR.
//
// GitCode does not expose a combined-status API the way GitHub does, so we
// derive contexts entirely from ProwJobs that have been created for this
// specific commit revision (keyed by OrgLabel/RepoLabel/PullLabel/SHA labels).
func (p *GitCodeProvider) headContexts(crc *CodeReviewCommon) ([]Context, error) {
	selector := map[string]string{
		kube.ProwJobTypeLabel: string(prowapi.PresubmitJob),
		kube.OrgLabel:         crc.Org,
		kube.RepoLabel:        crc.Repo,
		kube.PullLabel:        strconv.Itoa(crc.Number),
	}
	var pjs prowapi.ProwJobList
	if err := p.pjclientset.List(context.Background(), &pjs, ctrlruntimeclient.MatchingLabels(selector)); err != nil {
		return nil, fmt.Errorf("cannot list prowjobs with selector %v: %w", selector, err)
	}

	// Keep only the most recently created job per context name.
	latestPJs := make(map[string]*prowapi.ProwJob)
	for i := range pjs.Items {
		pj := &pjs.Items[i]
		// Only consider jobs targeting the current HEAD SHA.
		if pj.Spec.Refs == nil || pj.Spec.Refs.BaseSHA == "" {
			continue
		}
		if existing, ok := latestPJs[pj.Spec.Context]; ok &&
			existing.CreationTimestamp.After(pj.CreationTimestamp.Time) {
			continue
		}
		latestPJs[pj.Spec.Context] = pj
	}

	var contexts []Context
	for _, pj := range latestPJs {
		contexts = append(contexts, Context{
			Context:     githubql.String(pj.Spec.Context),
			Description: githubql.String(config.ContextDescriptionWithBaseSha(pj.Status.Description, pj.Spec.Refs.BaseSHA)),
			State:       githubql.StatusState(pj.Status.State),
		})
	}
	return contexts, nil
}

func (p *GitCodeProvider) mergePRs(sp subpool, prs []CodeReviewCommon, _ *threadSafePRSet) ([]CodeReviewCommon, error) {
	logger := p.logger.WithFields(logrus.Fields{
		"org":    sp.org,
		"repo":   sp.repo,
		"branch": sp.branch,
		"prs":    len(prs),
	})
	logger.Info("Merging GitCode subpool.")

	var merged []CodeReviewCommon
	var errs []error
	for _, pr := range prs {
		mergeType := types.MergeMerge // default
		if mm := p.prMergeMethod(&pr); mm != nil {
			mergeType = *mm
		}
		err := p.gc.MergePullRequest(sp.org, sp.repo, pr.Number, string(mergeType))
		if err != nil {
			errs = append(errs, fmt.Errorf("failed merging MR %d in %s/%s: %w", pr.Number, sp.org, sp.repo, err))
		} else {
			merged = append(merged, pr)
		}
	}
	return merged, utilerrors.NewAggregate(errs)
}

// GetTideContextPolicy returns a permissive context policy for GitCode.
//
// GitCode merge readiness is governed by the platform's submission rules
// (label votes, CI checks), not by Tide's context policy machinery. We return
// an always-optional checker so that Tide does not block merges on missing
// contexts.
func (p *GitCodeProvider) GetTideContextPolicy(org, repo, branch string, baseSHAGetter config.RefGetter, crc *CodeReviewCommon) (contextChecker, error) {
	return &gitcodeContextChecker{}, nil
}

func (p *GitCodeProvider) prMergeMethod(crc *CodeReviewCommon) *types.PullRequestMergeType {
	tideConfig := p.cfg().Tide
	// Respect explicit org/repo merge-type overrides from the shared Tide
	// configuration (same map used by the GitHub provider).
	orgRepo := crc.Org + "/" + crc.Repo
	for _, key := range []string{orgRepo, crc.Org} {
		if orgMerge, ok := tideConfig.MergeType[key]; ok {
			if orgMerge.MergeType != "" {
				mt := orgMerge.MergeType
				return &mt
			}
		}
	}
	mt := types.MergeMerge
	return &mt
}

func (p *GitCodeProvider) GetPresubmits(identifier, baseBranch string, baseSHAGetter config.RefGetter, headSHAGetters ...config.RefGetter) ([]config.Presubmit, error) {
	if p.inRepoConfigGetter != nil {
		return p.inRepoConfigGetter.GetPresubmits(identifier, baseBranch, baseSHAGetter, headSHAGetters...)
	}
	return p.cfg().GetPresubmitsStatic(identifier), nil
}

func (p *GitCodeProvider) GetChangedFiles(org, repo string, number int) ([]string, error) {
	files, err := p.gc.GetPullRequestFiles(org, repo, number)
	if err != nil {
		return nil, fmt.Errorf("failed getting changed files for MR %d in %s/%s: %w", number, org, repo, err)
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, f.Filename)
	}
	return names, nil
}

func (p *GitCodeProvider) refsForJob(sp subpool, prs []CodeReviewCommon) (prowapi.Refs, error) {
	endpoint := gitcode.DefaultEndpoint
	if p.cfg().Tide.GitCode != nil && p.cfg().Tide.GitCode.Endpoint != "" {
		endpoint = p.cfg().Tide.GitCode.Endpoint
	}
	// Derive base host from the API endpoint (strip "/api/v5" suffix).
	host := strings.TrimSuffix(endpoint, "/api/v5")
	host = strings.TrimRight(host, "/")

	refs := prowapi.Refs{
		Org:      sp.org,
		Repo:     sp.repo,
		BaseRef:  sp.branch,
		BaseSHA:  sp.sha,
		RepoLink: fmt.Sprintf("%s/%s/%s", host, sp.org, sp.repo),
		BaseLink: fmt.Sprintf("%s/%s/%s/tree/%s", host, sp.org, sp.repo, sp.sha),
	}
	for _, pr := range prs {
		refs.Pulls = append(refs.Pulls, prowapi.Pull{
			Number:     pr.Number,
			Title:      pr.Title,
			Author:     pr.AuthorLogin,
			SHA:        pr.HeadRefOID,
			HeadRef:    pr.HeadRefName,
			Link:       fmt.Sprintf("%s/%s/%s/pulls/%d", host, sp.org, sp.repo, pr.Number),
			CommitLink: fmt.Sprintf("%s/%s/%s/commit/%s", host, sp.org, sp.repo, pr.HeadRefOID),
			AuthorLink: fmt.Sprintf("%s/%s", host, pr.AuthorLogin),
		})
	}
	return refs, nil
}

func (p *GitCodeProvider) labelsAndAnnotations(instance string, jobLabels, jobAnnotations map[string]string, prs ...CodeReviewCommon) (labels, annotations map[string]string) {
	labels = make(map[string]string)
	annotations = make(map[string]string)
	for k, v := range jobLabels {
		labels[k] = v
	}
	for k, v := range jobAnnotations {
		annotations[k] = v
	}
	annotations[kube.GitCodeInstance] = instance
	if len(prs) == 1 {
		labels[kube.GitCodeRevision] = prs[0].HeadRefOID
	}
	return
}

func (p *GitCodeProvider) jobIsRequiredByTide(ps *config.Presubmit, _ *CodeReviewCommon) bool {
	return ps.ContextRequired() || ps.RunBeforeMerge
}
