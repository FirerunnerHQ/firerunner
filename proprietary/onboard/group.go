package onboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

// SyncGroup makes the organization's runner group named name open to the
// repositories the installation may access, public ones never: GitHub
// holds a public repository's jobs from the group itself
// (allows_public_repositories), so a repository made public later gets
// none either. An installation of all the organization's repositories gets
// a group open to every one that is not public, which takes in a new
// repository at once. The organization's default group is never changed:
// other runners serve it. An error is one to retry; a group GitHub will
// not create or change is a result, unavailable, with GitHub's reason.
func SyncGroup(ctx context.Context, gh Groups, installation int64, org, name string) (GroupResult, error) {
	repos, all, err := gh.Repositories(ctx, installation)
	if err != nil {
		return GroupResult{}, fmt.Errorf("list the installation's repositories: %w", err)
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
		return refusedOr(res, "list the organization's runner groups", err)
	}
	var group *RunnerGroup
	for i := range groups {
		if strings.EqualFold(groups[i].Name, name) {
			group = &groups[i]
		}
	}
	switch {
	case group == nil:
		g, err := gh.CreateRunnerGroup(ctx, installation, org, spec)
		if err != nil {
			return refusedOr(res, "create the runner group", err)
		}
		res.State, res.GroupID = "ready", g.ID
		return res, nil
	case group.Default:
		res.State, res.Reason = "unavailable", fmt.Sprintf("runner group %q is the organization's default, which onboarding never changes", group.Name)
		return res, nil
	}
	if group.Visibility != spec.Visibility || group.AllowsPublicRepositories {
		if err := gh.UpdateRunnerGroup(ctx, installation, org, group.ID, GroupSpec{Name: group.Name, Visibility: spec.Visibility}); err != nil {
			return refusedOr(res, "update the runner group", err)
		}
	}
	if spec.Visibility == "selected" {
		if err := gh.SetGroupRepositories(ctx, installation, org, group.ID, spec.Repositories); err != nil {
			return refusedOr(res, "set the runner group's repositories", err)
		}
	}
	res.State, res.GroupID = "ready", group.ID
	return res, nil
}

// refusedOr is GitHub refusing what the group needs (a plan without
// runner groups, the App's permission withdrawn) as a result, and any
// other failure, which a retry may get past, as an error.
func refusedOr(res GroupResult, what string, err error) (GroupResult, error) {
	var se *StatusError
	refused := false
	if errors.As(err, &se) {
		switch se.Code {
		case http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity:
			refused = !se.RateLimited
		}
	}
	if !refused {
		return GroupResult{}, fmt.Errorf("%s: %w", what, err)
	}
	res.State = "unavailable"
	res.Reason = fmt.Sprintf("GitHub refused to %s: %s", what, se.Message)
	if se.Message == "" {
		res.Reason = fmt.Sprintf("GitHub refused to %s: HTTP %d", what, se.Code)
	}
	return res, nil
}
