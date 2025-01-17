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

package main

import (
	"flag"
	"github.com/akatrevorjay/git-appraise/repository"
	"github.com/akatrevorjay/git-phabricator-mirror/mirror"
	"github.com/op/go-logging"
	"os"
	"path/filepath"
	"time"
)

var searchDir = flag.String("search_dir", "/var/repo", "Directory under which to search for git repos")
var syncToRemote = flag.Bool("sync_to_remote", false, "Sync the local repos (including git notes) to their remotes")
var syncPeriod = flag.Int("sync_period", 30, "Expected number of seconds between subsequent syncs of a repo.")

var logger = logging.MustGetLogger("mirror")

func orPanic(err error) {
	if err == nil {
		return
	}
	logger.Panic(err)
	panic(err)
}

func orFatalf(err error) {
	if err == nil {
		return
	}
	logger.Fatalf("Error: %s", err.Error())
}

func orErrorf(err error) {
	if err == nil {
		return
	}
	logger.Errorf("Error: %s", err.Error())
}

func findRepos(searchDir string) ([]repository.Repo, error) {
	// This method finds repos by recursively traversing the given directory,
	// and looking for any git repos.
	var repos []repository.Repo
	filepath.Walk(searchDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			gitRepo, err := repository.NewGitRepo(path)
			if err == nil {
				repos = append(repos, gitRepo)
				// Since we have found a git repo, we don't need to
				// traverse any of its child directories.
				return filepath.SkipDir
			}
		}
		return nil
	})
	return repos, nil
}

// InitLoggers initialize loggers
func InitLoggers(verbosity int) (err error) {
	var format = logging.MustStringFormatter(
		`%{color}%{time:15:04:05.000} %{module} %{longfunc}: %{color:bold}%{message} %{color:reset}%{color}@%{shortfile} %{color}#%{level}%{color:reset}`,
	)

	backend := logging.NewLogBackend(os.Stderr, "", 0)
	formatter := logging.NewBackendFormatter(backend, format)
	//logging.SetBackend(formatter)

	leveledBackend := logging.AddModuleLevel(formatter)

	switch {
	case verbosity == 1:
		logging.SetLevel(logging.INFO, "")
		leveledBackend.SetLevel(logging.INFO, "")
	case verbosity >= 2:
		logging.SetLevel(logging.DEBUG, "")
		leveledBackend.SetLevel(logging.DEBUG, "")
	}

	logging.SetBackend(leveledBackend)
	return
}

func main() {
	InitLoggers(9)

	flag.Parse()
	// We want to always start processing new repos that are added after the binary has started,
	// so we need to run the findRepos method in an infinite loop.

	ticker := time.Tick(time.Duration(*syncPeriod) * time.Second)
	for {
		repos, err := findRepos(*searchDir)
		if err != nil {
			logger.Panic(err.Error())
		}
		for _, repo := range repos {
			mirror.Repo(repo, *syncToRemote)
		}
		if *syncToRemote {
			<-ticker
		}
	}
}
