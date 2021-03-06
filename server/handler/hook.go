package handler

import (
	"net/http"
	"strings"

	"github.com/drone/drone/plugin/remote"
	"github.com/drone/drone/server/datastore"
	"github.com/drone/drone/server/worker"
	"github.com/drone/drone/shared/build/script"
	"github.com/drone/drone/shared/httputil"
	"github.com/drone/drone/shared/model"
	"github.com/goji/context"
	"github.com/zenazn/goji/web"
)

// PostHook accepts a post-commit hook and parses the payload
// in order to trigger a build. The payload is specified to the
// remote system (ie GitHub) and will therefore get parsed by
// the appropriate remote plugin.
//
//     GET /api/hook/:host
//
func PostHook(c web.C, w http.ResponseWriter, r *http.Request) {
	var ctx = context.FromC(c)
	var host = c.URLParams["host"]
	var remote = remote.Lookup(host)
	if remote == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// parse the hook payload
	hook, err := remote.ParseHook(r)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// in some cases we have neither a hook nor error. An example
	// would be GitHub sending a ping request to the URL, in which
	// case we'll just exit quiely with an 'OK'
	if hook == nil || strings.Contains(hook.Message, "[CI SKIP]") {
		w.WriteHeader(http.StatusOK)
		return
	}

	// fetch the repository from the database
	repo, err := datastore.GetRepoName(ctx, remote.GetHost(), hook.Owner, hook.Repo)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if repo.Active == false ||
		(repo.PostCommit == false && len(hook.PullRequest) == 0) ||
		(repo.PullRequest == false && len(hook.PullRequest) != 0) {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// fetch the user from the database that owns this repo
	user, err := datastore.GetUser(ctx, repo.UserID)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// featch the .drone.yml file from the database
	yml, err := remote.GetScript(user, repo, hook)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// verify the commit hooks branch matches the list of approved
	// branches (unless it is a pull request). Note that we don't really
	// care if parsing the yaml fails here.
	s, _ := script.ParseBuild(string(yml), map[string]string{})
	if len(hook.PullRequest) == 0 && !s.MatchBranch(hook.Branch) {
		w.WriteHeader(http.StatusOK)
		return
	}

	commit := model.Commit{
		RepoID:      repo.ID,
		Status:      model.StatusEnqueue,
		Sha:         hook.Sha,
		Branch:      hook.Branch,
		PullRequest: hook.PullRequest,
		Timestamp:   hook.Timestamp,
		Message:     hook.Message,
		Config:      string(yml),
	}
	commit.SetAuthor(hook.Author)

	// inserts the commit into the database
	if err := datastore.PostCommit(ctx, &commit); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	owner, err := datastore.GetUser(ctx, repo.UserID)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// drop the items on the queue
	go worker.Do(ctx, &worker.Work{
		User:   owner,
		Repo:   repo,
		Commit: &commit,
		Host:   httputil.GetURL(r),
	})

	w.WriteHeader(http.StatusOK)
}
