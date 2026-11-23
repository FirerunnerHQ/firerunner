package onboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// Repository is a repository an installation may access.
type Repository struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
	// Visibility is public, private or internal.
	Visibility string `json:"visibility"`
}

// public reports whether anyone may read the repository: its jobs, and
// the pull requests of anyone's forks, would run on the tenant's runners.
func (r Repository) public() bool {
	return r.Visibility == "public" || r.Visibility == "" && !r.Private
}

// RunnerGroup is an organization's runner group.
type RunnerGroup struct {
	ID                       int64  `json:"id"`
	Name                     string `json:"name"`
	Default                  bool   `json:"default"`
	Visibility               string `json:"visibility"`
	AllowsPublicRepositories bool   `json:"allows_public_repositories"`
}

// GroupSpec is a runner group as onboarding keeps it.
type GroupSpec struct {
	Name string
	// Visibility is private (all of the organization's repositories that
	// are not public) or selected (Repositories).
	Visibility   string
	Repositories []int64
}

// Groups is what keeping a runner group needs of GitHub; a *GitHub.
type Groups interface {
	Repositories(ctx context.Context, installation int64) ([]Repository, bool, error)
	RunnerGroups(ctx context.Context, installation int64, org string) ([]RunnerGroup, error)
	CreateRunnerGroup(ctx context.Context, installation int64, org string, spec GroupSpec) (RunnerGroup, error)
	UpdateRunnerGroup(ctx context.Context, installation int64, org string, id int64, spec GroupSpec) error
	SetGroupRepositories(ctx context.Context, installation int64, org string, id int64, repositories []int64) error
}

// GroupResult is what keeping an installation's runner group came to: for
// the first-run view, and the installation's row.
type GroupResult struct {
	// State is ready, or unavailable: GitHub refused the group, and Reason
	// says why (a plan without runner groups, or the App's permission).
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	// GroupID is the group's.
	GroupID int64 `json:"group_id,omitempty"`
	// All is a group open to every repository of the organization that is
	// not public; else Served are the repositories it is open to.
	All    bool `json:"all,omitempty"`
	Served int  `json:"served"`
	// Public are the installation's public repositories, which the group
	// is not open to.
	Public []string `json:"public,omitempty"`
}

// SyncGroup makes the runner group onboarding created in the organization
// (owned, 0 before it has; created now if so, named name) open to the
// repositories the installation may access, public ones never: GitHub
// holds a public repository's jobs from the group itself
// (allows_public_repositories), so a repository made public later gets
// none either. An installation of all the organization's repositories gets
// a group open to every one that is not public, which takes in a new
// repository at once.
//
// A group onboarding did not create is never changed, the organization's
// default above all, even under the name: its runners and repositories are
// the organization's own. An error is one to retry; GitHub refusing what
// the group needs (a plan without runner groups, the App's permission
// withdrawn, the name taken) is a result, unavailable, with the reason.
func SyncGroup(ctx context.Context, gh Groups, installation int64, org, name string, owned int64) (GroupResult, error) {
	repos, all, err := gh.Repositories(ctx, installation)
	if err != nil {
		return refusedOr(GroupResult{}, "give the installation's repositories (has it granted the App the organization's self-hosted runners permission?)", err, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity)
	}
	spec := GroupSpec{Name: name, Visibility: "private"}
	res := GroupResult{All: all}
	for _, r := range repos {
		switch {
		case r.public():
			res.Public = append(res.Public, r.FullName)
		case !all:
			spec.Repositories = append(spec.Repositories, r.ID)
		}
	}
	if !all {
		spec.Visibility = "selected"
	}
	res.Served = len(repos) - len(res.Public)

	groups, err := gh.RunnerGroups(ctx, installation, org)
	if err != nil {
		return refusedOr(res, "list the organization's runner groups", err, http.StatusForbidden, http.StatusNotFound)
	}
	var mine, named *RunnerGroup
	for i := range groups {
		if owned != 0 && groups[i].ID == owned && !groups[i].Default {
			mine = &groups[i]
		}
		if strings.EqualFold(groups[i].Name, name) {
			named = &groups[i]
		}
	}
	if mine == nil {
		// None yet, or the organization deleted it.
		if named != nil {
			res.State, res.Reason = "unavailable", fmt.Sprintf("the organization has a runner group named %q that onboarding did not create; rename or delete it, and onboarding creates its own", named.Name)
			return res, nil
		}
		g, err := gh.CreateRunnerGroup(ctx, installation, org, spec)
		if err != nil {
			return refusedOr(res, "create the runner group", err, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity)
		}
		res.State, res.GroupID = "ready", g.ID
		return res, nil
	}
	if named != nil && named != mine {
		res.State, res.Reason = "unavailable", fmt.Sprintf("onboarding's runner group was renamed %q, and another group has its name %q", mine.Name, named.Name)
		return res, nil
	}
	if mine.Name != name || mine.Visibility != spec.Visibility || mine.AllowsPublicRepositories {
		// Renamed back too: an entry without its id (runner_group_id)
		// finds the group by name.
		if err := gh.UpdateRunnerGroup(ctx, installation, org, mine.ID, GroupSpec{Name: name, Visibility: spec.Visibility}); err != nil {
			return refusedOr(res, "update the runner group", err, http.StatusForbidden)
		}
	}
	if spec.Visibility == "selected" {
		// A repository deleted since it was listed fails this with 404 or
		// 422: an error, and the retry lists the repositories again.
		if err := gh.SetGroupRepositories(ctx, installation, org, mine.ID, spec.Repositories); err != nil {
			return refusedOr(res, "set the runner group's repositories", err, http.StatusForbidden)
		}
	}
	res.State, res.GroupID = "ready", mine.ID
	return res, nil
}

// refusedOr is GitHub refusing what the group needs, with one of codes and
// not to be retried, as a result; any other failure, which a retry may get
// past, as an error.
func refusedOr(res GroupResult, what string, err error, codes ...int) (GroupResult, error) {
	var se *StatusError
	if !errors.As(err, &se) || se.Retryable() || !slices.Contains(codes, se.Code) {
		return GroupResult{}, fmt.Errorf("%s: %w", what, err)
	}
	res.State = "unavailable"
	res.Reason = fmt.Sprintf("GitHub refused to %s: %s", what, se.Message)
	if se.Message == "" {
		res.Reason = fmt.Sprintf("GitHub refused to %s: HTTP %d", what, se.Code)
	}
	return res, nil
}
