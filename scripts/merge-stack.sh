#!/usr/bin/env bash
# merge-stack.sh — merge a PR that other PRs are stacked on, without losing
# them or leaving their issues open. Dry run by default: prints the plan and
# changes nothing until rerun with --yes.
#
# For PR N (which must target the default branch) it:
#   1. retargets every open PR based on N's branch to the default branch,
#      because deleting a base branch closes, rather than retargets, its PRs;
#   2. merges N with --merge --delete-branch;
#   3. closes the issues that PRs merged into N's branch (or, recursively, into
#      their branches) reference with Closes/Fixes/Resolves #M, with a comment
#      linking the merge: GitHub closes those only for PRs merged into the
#      default branch.
#
# Usage: scripts/merge-stack.sh [--yes] PR
#   --yes  execute the plan (default: print it only)
# Needs gh, authenticated for the repository of the current directory.
set -euo pipefail

usage() { sed -n '2,17p' "$0" | sed 's/^# \{0,1\}//'; }

yes=0
pr=""
for arg in "$@"; do
	case "$arg" in
	--yes) yes=1 ;;
	-h | --help)
		usage
		exit 0
		;;
	-*)
		echo "unknown option: $arg" >&2
		exit 2
		;;
	*)
		[ -z "$pr" ] || {
			echo "only one PR at a time" >&2
			exit 2
		}
		pr="${arg#\#}"
		;;
	esac
done
case "$pr" in
'' | *[!0-9]*)
	usage >&2
	exit 2
	;;
esac

# Issue numbers a PR body closes. GitHub's keywords, case-insensitive, with an
# optional colon; references to other repositories (owner/repo#N) are skipped.
refs_jq='[(.body // "") | scan("(?i)\\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?):?\\s+#([0-9]+)") | .[0]] | unique | .[]'

# Every gh read is captured in a variable before use, so that a failed call
# stops the script (set -e) instead of reading as "no PRs" or "no issues".
main="$(gh repo view --json defaultBranchRef --jq .defaultBranchRef.name)"
info="$(gh pr view "$pr" --json state,headRefName,baseRefName,url \
	--jq '[.state, .headRefName, .baseRefName, .url] | @tsv')"
IFS=$'\t' read -r state head base url <<<"$info"

if [ "$state" != OPEN ]; then
	echo "#$pr is $state, not open" >&2
	exit 1
fi
if [ "$base" != "$main" ]; then
	echo "#$pr targets $base, not $main: merge the bottom of the stack first" >&2
	exit 1
fi

# Issues the merge itself closes, as N lands in the default branch.
own="$(gh pr view "$pr" --json body --jq "$refs_jq")"
own=" $(tr '\n' ' ' <<<"$own")"

# list_prs <base> <state>: "number<TAB>head branch" per PR.
list_prs() {
	gh pr list --base "$1" --state "$2" --limit 200 \
		--json number,headRefName --jq '.[] | [.number, .headRefName] | @tsv'
}

# 1. Open PRs on N's branch.
children=()
list="$(list_prs "$head" open)"
while IFS=$'\t' read -r n branch; do
	[ -n "$n" ] && children+=("$n	$branch")
done <<<"$list"

# 3. Issues referenced by PRs merged into N's branch, and into theirs.
closes=() # "issue<TAB>pr<TAB>branch it was merged into"
seen=" "      # issues already planned
visited=" "   # branches already listed
queue=("$head")
while [ "${#queue[@]}" -gt 0 ]; do
	branch="${queue[0]}"
	queue=("${queue[@]:1}")
	case "$visited" in *" $branch "*) continue ;; esac
	visited="$visited$branch "
	list="$(list_prs "$branch" merged)"
	while IFS=$'\t' read -r n sub; do
		[ -n "$n" ] || continue
		queue+=("$sub")
		refs="$(gh pr view "$n" --json body --jq "$refs_jq")"
		for issue in $refs; do
			case "$own$seen" in *" $issue "*) continue ;; esac
			seen="$seen$issue "
			closes+=("$issue	$n	$branch")
		done
	done <<<"$list"
done

issue_state() { gh issue view "$1" --json state --jq .state; }

echo "#$pr: $head -> $main ($url)"
for c in ${children[@]+"${children[@]}"}; do
	echo "retarget  #${c%%	*} (${c#*	}) from $head to $main"
done
echo "merge     #$pr with --merge --delete-branch"
for c in ${closes[@]+"${closes[@]}"}; do
	IFS=$'\t' read -r issue n branch <<<"$c"
	st="$(issue_state "$issue")"
	if [ "$st" = OPEN ]; then
		echo "close     #$issue (from #$n, merged into $branch)"
	else
		echo "skip      #$issue (from #$n): already closed"
	fi
done

if [ "$yes" != 1 ]; then
	echo
	echo "dry run: nothing changed. Rerun with --yes to do this."
	exit 0
fi

echo
for c in ${children[@]+"${children[@]}"}; do
	gh pr edit "${c%%	*}" --base "$main"
done
gh pr merge "$pr" --merge --delete-branch
sha="$(gh pr view "$pr" --json mergeCommit --jq '.mergeCommit.oid // ""')"
for c in ${closes[@]+"${closes[@]}"}; do
	IFS=$'\t' read -r issue n branch <<<"$c"
	st="$(issue_state "$issue")"
	[ "$st" = OPEN ] || continue
	gh issue close "$issue" --comment "Fixed by #$n, which was merged into \`$branch\`; that landed in \`$main\` with #$pr${sha:+ ($sha)}."
done
echo "done: merged #$pr"
