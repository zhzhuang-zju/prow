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

// Package gitcode provides a client and types for interacting with the GitCode
// REST API (https://gitcode.com). GitCode is a code hosting platform that
// provides a GitHub-compatible REST API.
package gitcode

import "time"

// MergeRequest represents a GitCode pull request / merge request.
// The JSON field names match the GitCode REST API v5 response format.
type MergeRequest struct {
	// ID is the global unique identifier of the MR.
	ID int `json:"id"`
	// Number is the per-repository sequence number of the MR (iid).
	Number int `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	// State is the current state of the MR, e.g. "open", "closed", "merged".
	State string `json:"state"`
	// Mergeable indicates whether the MR can be cleanly merged.
	// nil means the mergeability has not been computed yet.
	Mergeable *bool `json:"mergeable"`

	Head HeadRef `json:"head"`
	Base BaseRef `json:"base"`

	User      User      `json:"user"`
	UpdatedAt time.Time `json:"updated_at"`
	Labels    []Label   `json:"labels"`
}

// HeadRef describes the source branch of a MergeRequest.
type HeadRef struct {
	SHA  string   `json:"sha"`
	Ref  string   `json:"ref"`
	Repo HeadRepo `json:"repo"`
}

// HeadRepo is the repository information embedded in HeadRef.
type HeadRepo struct {
	Name  string    `json:"name"`
	Owner RepoOwner `json:"owner"`
}

// RepoOwner is a minimal owner (org/user) description returned by the API.
type RepoOwner struct {
	Login string `json:"login"`
}

// BaseRef describes the target branch of a MergeRequest.
type BaseRef struct {
	SHA string `json:"sha"`
	Ref string `json:"ref"`
}

// User is a GitCode user reference.
type User struct {
	Login string `json:"login"`
}

// Label is a GitCode label attached to a MergeRequest.
type Label struct {
	Name string `json:"name"`
}

// Branch represents a repository branch as returned by the GitCode API.
type Branch struct {
	Name   string       `json:"name"`
	Commit BranchCommit `json:"commit"`
}

// BranchCommit holds the SHA of the latest commit on a branch.
type BranchCommit struct {
	SHA string `json:"sha"`
}

// File is an entry in the list of files changed by a MergeRequest.
type File struct {
	Filename string `json:"filename"`
}
