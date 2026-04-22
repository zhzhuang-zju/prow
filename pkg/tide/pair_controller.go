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
	"strings"

	"github.com/sirupsen/logrus"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/prow/pkg/config"
	configflagutil "sigs.k8s.io/prow/pkg/flagutil/config"
	git "sigs.k8s.io/prow/pkg/git/v2"
	"sigs.k8s.io/prow/pkg/github"
	"sigs.k8s.io/prow/pkg/io"
	"sigs.k8s.io/prow/pkg/moonraker"
	"sigs.k8s.io/prow/pkg/tide/history"
)

// NewPairController creates a Tide Controller backed by a PairProvider that
// synchronises merges across GitHub (first) and GitCode (second).
//
// Both platforms receive identical commits via direct git push — no
// platform-side merge commits are created.  GitHub is always the primary
// (written first); GitCode is written second.
//
// Prerequisites in the Prow config:
//   - tide.queries must list the GitHub repos to watch (standard Tide queries).
//   - tide.gitcode must list the GitCode repos to watch.
//   - tide.pair must supply token paths for authenticated git push.
func NewPairController(
	ghcSync, ghcStatus github.Client,
	mgr manager,
	cfgAgent *config.Agent,
	gc git.ClientFactory,
	maxRecordsPerPool int,
	opener io.Opener,
	historyURI, statusURI string,
	logger *logrus.Entry,
	usesGitHubAppsAuth bool,
	configOptions configflagutil.ConfigOptions,
) (*Controller, error) {
	if logger == nil {
		logger = logrus.NewEntry(logrus.StandardLogger())
	}

	cfg := cfgAgent.Config
	pairCfg := cfg().Tide.Pair
	if pairCfg == nil {
		return nil, fmt.Errorf("tide.pair config block is required for NewPairController")
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

	// ----------------------------------------------------------------
	// InRepoConfig getter (shared by both sub-providers)
	// ----------------------------------------------------------------
	var ircg config.InRepoConfigGetter
	if configOptions.MoonrakerAddress != "" {
		moonrakerClient, err := moonraker.NewClient(configOptions.MoonrakerAddress, cfgAgent)
		if err != nil {
			return nil, fmt.Errorf("error creating moonraker client: %w", err)
		}
		ircg = moonrakerClient
	} else {
		ircg, err = config.NewInRepoConfigCache(configOptions.InRepoConfigCacheSize, cfgAgent, gc)
		if err != nil {
			return nil, fmt.Errorf("failed creating inrepoconfig cache: %w", err)
		}
	}

	// ----------------------------------------------------------------
	// GitHub sub-provider
	// ----------------------------------------------------------------
	mergeChecker := newMergeChecker(cfg, ghcSync)
	githubProvider := newGitHubProvider(logger, ghcSync, gc, cfg, mergeChecker, usesGitHubAppsAuth)

	// ----------------------------------------------------------------
	// GitCode sub-provider
	// ----------------------------------------------------------------
	gitcodeProvider := newGitCodeProvider(logger, cfg, mgr.GetClient(), ircg)

	// ----------------------------------------------------------------
	// Authenticated remote URL builders for git push
	// ----------------------------------------------------------------
	ghToken, err := readTokenFile(pairCfg.GitHubTokenPath)
	if err != nil {
		return nil, fmt.Errorf("pair: read GitHub token from %q: %w", pairCfg.GitHubTokenPath, err)
	}
	gcTokenPath := pairCfg.GitCodeTokenPath
	if gcTokenPath == "" && cfg().Tide.GitCode != nil {
		gcTokenPath = cfg().Tide.GitCode.TokenPath
	}
	gcToken, err := readTokenFile(gcTokenPath)
	if err != nil {
		return nil, fmt.Errorf("pair: read GitCode token from %q: %w", gcTokenPath, err)
	}

	ghHost := pairCfg.GitHubHostOrDefault()
	gcHost := pairCfg.GitCodeHostOrDefault()

	firstRemoteURL := buildHTTPSRemoteURL(ghHost, ghToken)
	secondRemoteURL := buildHTTPSRemoteURL(gcHost, gcToken)

	// ----------------------------------------------------------------
	// PairProvider
	// ----------------------------------------------------------------
	pairProvider := NewPairProvider(
		githubProvider,
		gitcodeProvider,
		gc,
		firstRemoteURL,
		secondRemoteURL,
		logger,
	)

	// ----------------------------------------------------------------
	// Status controller (GitHub-side only — status contexts live on GitHub)
	// ----------------------------------------------------------------
	sc, err := newStatusController(ctx, logger, ghcStatus, mgr, gc, cfg, opener, statusURI, mergeChecker, usesGitHubAppsAuth, statusUpdate)
	if err != nil {
		return nil, err
	}
	go sc.run()

	// ----------------------------------------------------------------
	// Sync controller
	// ----------------------------------------------------------------
	syncCtrl, err := newSyncController(ctx, logger, mgr, pairProvider, cfg, gc, hist, usesGitHubAppsAuth, statusUpdate)
	if err != nil {
		return nil, err
	}
	return &Controller{syncCtrl: syncCtrl, statusCtrl: sc}, nil
}

// newGitCodeProviderFromPJClient is an internal helper used when we already
// have a ctrlruntimeclient.Client (avoids repeating the mgr.GetClient() call).
func newGitCodeProviderFromPJClient(logger *logrus.Entry, cfg config.Getter, pjclient ctrlruntimeclient.Client, ircg config.InRepoConfigGetter) *GitCodeProvider {
	return newGitCodeProvider(logger, cfg, pjclient, ircg)
}

// readTokenFile reads a token from a file, trimming whitespace.
// Returns an error if the path is empty or the file cannot be read.
func readTokenFile(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("token path is empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// buildHTTPSRemoteURL returns a RemoteURLFunc that embeds the given token into
// the HTTPS remote URL: "https://oauth2:<token>@<host>/<org>/<repo>.git".
func buildHTTPSRemoteURL(host, token string) RemoteURLFunc {
	return func(org, repo string) (string, error) {
		if token == "" {
			return "", fmt.Errorf("empty token for remote URL %s/%s/%s", host, org, repo)
		}
		return fmt.Sprintf("https://oauth2:%s@%s/%s/%s.git", token, host, org, repo), nil
	}
}
