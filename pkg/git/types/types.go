/*
Copyright 2022 The Kubernetes Authors.

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

// Package types stores types used by all git clients
package types

// PullRequestMergeType enumerates the types of merges used by Prow for either GitHub or Gerrit API
type PullRequestMergeType string

// Possible types of merges for the GitHub merge API
const (
	MergeMerge       PullRequestMergeType = "merge"
	MergeRebase      PullRequestMergeType = "rebase"
	MergeSquash      PullRequestMergeType = "squash"
	MergeIfNecessary PullRequestMergeType = "ifNecessary"
	// MergeGitPush performs a fast-forward push of the PR's HEAD commit directly
	// to the base branch via `git push`, without invoking any platform merge API.
	// The PR branch must be up-to-date with the base branch (fast-forward only).
	// This preserves the original commit SHAs exactly as they appear in the PR.
	MergeGitPush PullRequestMergeType = "gitPush"
)
