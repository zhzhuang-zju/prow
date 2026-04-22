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

package gitcode

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// DefaultEndpoint is the public GitCode API base URL.
	DefaultEndpoint = "https://gitcode.com/api/v5"

	// defaultTimeout is the HTTP client timeout for all GitCode API requests.
	defaultTimeout = 30 * time.Second
)

// Client is the interface for interacting with the GitCode REST API.
// All operations required by the Tide GitCodeProvider are exposed here so that
// the interface can be easily mocked in unit tests.
type Client interface {
	// ListOpenMergeRequests returns all open merge requests for the given
	// org (owner) and repo. The caller must page through results if needed;
	// the implementation fetches all pages internally.
	ListOpenMergeRequests(org, repo string) ([]MergeRequest, error)

	// GetBranchSHA returns the HEAD commit SHA of the specified branch.
	GetBranchSHA(org, repo, branch string) (string, error)

	// MergePullRequest merges the merge request identified by number using
	// the given merge method ("merge", "squash", or "rebase").
	MergePullRequest(org, repo string, number int, mergeMethod string) error

	// GetPullRequestFiles returns the list of files changed by the merge
	// request identified by number.
	GetPullRequestFiles(org, repo string, number int) ([]File, error)
}

// client is the default implementation of Client backed by the GitCode REST API.
type client struct {
	endpoint   string
	token      string
	httpClient *http.Client
}

// NewClient creates a GitCode API client.
//
//   - endpoint is the base URL of the GitCode API (defaults to DefaultEndpoint
//     when empty).
//   - token is a personal access token used to authenticate API requests.
func NewClient(endpoint, token string) Client {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	// Trim any trailing slash so we can safely append paths.
	endpoint = strings.TrimRight(endpoint, "/")
	return &client{
		endpoint:   endpoint,
		token:      token,
		httpClient: &http.Client{Timeout: defaultTimeout},
	}
}

// get issues a GET request to the given GitCode API path (relative to the
// configured endpoint) and decodes the JSON response body into dest.
func (c *client) get(path string, dest interface{}, query url.Values) error {
	reqURL := c.endpoint + path
	if len(query) > 0 {
		reqURL = reqURL + "?" + query.Encode()
	}

	req, err := http.NewRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("creating request for %s: %w", reqURL, err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "token "+c.token)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("executing GET %s: %w", reqURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GET %s returned HTTP %d: %s", reqURL, resp.StatusCode, string(body))
	}

	if err := json.NewDecoder(resp.Body).Decode(dest); err != nil {
		return fmt.Errorf("decoding response from %s: %w", reqURL, err)
	}
	return nil
}

// post issues a POST/PUT request with a JSON body and optionally decodes the response.
func (c *client) put(path string, bodyJSON string, dest interface{}) error {
	reqURL := c.endpoint + path
	req, err := http.NewRequest(http.MethodPost, reqURL, strings.NewReader(bodyJSON))
	if err != nil {
		return fmt.Errorf("creating request for %s: %w", reqURL, err)
	}
	req.Method = http.MethodPut
	if c.token != "" {
		req.Header.Set("Authorization", "token "+c.token)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("executing PUT %s: %w", reqURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("PUT %s returned HTTP %d: %s", reqURL, resp.StatusCode, string(body))
	}

	if dest != nil {
		if err := json.NewDecoder(resp.Body).Decode(dest); err != nil {
			return fmt.Errorf("decoding response from %s: %w", reqURL, err)
		}
	}
	return nil
}

// ListOpenMergeRequests implements Client.
//
// GitCode API: GET /repos/{owner}/{repo}/pulls?state=open&page=N&per_page=100
func (c *client) ListOpenMergeRequests(org, repo string) ([]MergeRequest, error) {
	var all []MergeRequest
	page := 1
	for {
		q := url.Values{}
		q.Set("state", "open")
		q.Set("per_page", "100")
		q.Set("page", fmt.Sprintf("%d", page))

		var batch []MergeRequest
		path := fmt.Sprintf("/repos/%s/%s/pulls", org, repo)
		if err := c.get(path, &batch, q); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			break
		}
		page++
	}
	return all, nil
}

// GetBranchSHA implements Client.
//
// GitCode API: GET /repos/{owner}/{repo}/branches/{branch}
func (c *client) GetBranchSHA(org, repo, branch string) (string, error) {
	var b Branch
	path := fmt.Sprintf("/repos/%s/%s/branches/%s", org, repo, branch)
	if err := c.get(path, &b, nil); err != nil {
		return "", err
	}
	return b.Commit.SHA, nil
}

// MergePullRequest implements Client.
//
// GitCode API: PUT /repos/{owner}/{repo}/pulls/{number}/merge
func (c *client) MergePullRequest(org, repo string, number int, mergeMethod string) error {
	if mergeMethod == "" {
		mergeMethod = "merge"
	}
	body := fmt.Sprintf(`{"merge_method":%q}`, mergeMethod)
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/merge", org, repo, number)
	return c.put(path, body, nil)
}

// GetPullRequestFiles implements Client.
//
// GitCode API: GET /repos/{owner}/{repo}/pulls/{number}/files
func (c *client) GetPullRequestFiles(org, repo string, number int) ([]File, error) {
	var files []File
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/files", org, repo, number)
	if err := c.get(path, &files, nil); err != nil {
		return nil, err
	}
	return files, nil
}
