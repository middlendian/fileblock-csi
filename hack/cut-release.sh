#!/usr/bin/env bash
#
# hack/cut-release.sh vX.Y.Z[-prerelease]
#
# Cuts a release **as a PR**. Assumes the maintainer has already edited
# CHANGELOG.md to promote the current `## [Unreleased]` section to
# `## [X.Y.Z] - YYYY-MM-DD` (and added a fresh empty `## [Unreleased]`
# above it, plus the `[X.Y.Z]` link reference at the bottom).
#
# What this script does:
#   1. Bumps `deploy/kustomize/base/kustomization.yaml` newTag to `vX.Y.Z`
#      (overwriting whatever the previous release's tag was).
#   2. Creates a `release/vX.Y.Z` branch with one commit on it
#      (CHANGELOG promotion + kustomization bump), subject
#      `release: vX.Y.Z`.
#   3. Pushes the branch and opens a PR via `gh pr create`.
#
# When that PR is squash-merged to `main`, the squash commit subject is
# `release: vX.Y.Z (#N)`, which `tag-and-release.yml` detects and uses
# to create the `vX.Y.Z` tag and run the release pipeline.
set -euo pipefail

err() { echo "ERROR: $*" >&2; exit 1; }

VERSION="${1:-}"
[ -n "$VERSION" ] || err "usage: $0 vX.Y.Z[-prerelease]"
case "$VERSION" in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *) err "VERSION must look like vX.Y.Z[-...] (got: $VERSION)" ;;
esac
SEMVER="${VERSION#v}"

KUSTOMIZATION="deploy/kustomize/base/kustomization.yaml"
[ -f "$KUSTOMIZATION" ] || err "$KUSTOMIZATION not found — run from repo root"

BRANCH="release/$VERSION"

# Branch / sync / tag preconditions.
CURRENT_BRANCH="$(git rev-parse --abbrev-ref HEAD)"
[ "$CURRENT_BRANCH" = "main" ] \
  || err "must be on 'main' (currently on '$CURRENT_BRANCH')"
git fetch origin main --quiet
[ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ] \
  || err "local 'main' is not at origin/main — pull/rebase first"
git rev-parse "$VERSION" >/dev/null 2>&1 \
  && err "tag $VERSION already exists locally"
git ls-remote --exit-code --tags origin "$VERSION" >/dev/null 2>&1 \
  && err "tag $VERSION already exists on origin"
git rev-parse "refs/heads/$BRANCH" >/dev/null 2>&1 \
  && err "local branch $BRANCH already exists"
git ls-remote --exit-code --heads origin "$BRANCH" >/dev/null 2>&1 \
  && err "remote branch $BRANCH already exists"

# Working tree must be clean except possibly CHANGELOG.md.
DIRTY_OTHER="$(git status --porcelain | awk '$2 != "CHANGELOG.md" {print}')"
[ -z "$DIRTY_OTHER" ] || {
  echo "ERROR: working tree has changes other than CHANGELOG.md:" >&2
  git status --short >&2
  exit 1
}

# CHANGELOG.md must contain the promoted section.
grep -qE "^## \[$SEMVER\] - [0-9]{4}-[0-9]{2}-[0-9]{2}" CHANGELOG.md \
  || err "CHANGELOG.md does not contain a '## [$SEMVER] - YYYY-MM-DD' heading.
       Edit CHANGELOG.md to promote '## [Unreleased]' to '## [$SEMVER] - $(date +%Y-%m-%d)'
       (add a fresh empty '## [Unreleased]' above it, and a '[$SEMVER]:' link
       reference at the bottom), then re-run."

# Kustomization must have exactly one newTag line and it must not already
# be the new version (which would mean a prior run got stuck mid-flight).
NEWTAG_LINES="$(grep -cE '^[[:space:]]+newTag: ' "$KUSTOMIZATION" || true)"
[ "$NEWTAG_LINES" = "1" ] \
  || err "$KUSTOMIZATION must have exactly one 'newTag: …' line (found: $NEWTAG_LINES)"
CURRENT_TAG="$(awk '/^[[:space:]]+newTag:/ {print $2; exit}' "$KUSTOMIZATION")"
[ "$CURRENT_TAG" != "$VERSION" ] \
  || err "$KUSTOMIZATION already has newTag: $VERSION — nothing to bump"

echo "==> Creating branch $BRANCH"
git checkout -b "$BRANCH"

echo "==> Bumping $KUSTOMIZATION newTag: $CURRENT_TAG -> $VERSION"
sed -i -E "s|^([[:space:]]+)newTag: $CURRENT_TAG\$|\\1newTag: $VERSION|" "$KUSTOMIZATION"
git diff --quiet -- "$KUSTOMIZATION" \
  && err "kustomization sed produced no change — check the file by hand"

echo "==> Committing release: $VERSION"
git add CHANGELOG.md "$KUSTOMIZATION"
git commit -m "release: $VERSION"

echo "==> Pushing $BRANCH"
git push -u origin "$BRANCH"

echo "==> Opening PR"
gh pr create \
  --base main \
  --head "$BRANCH" \
  --title "release: $VERSION" \
  --body "$(cat <<EOF
## Summary

Cuts release **$VERSION**.

- Promotes \`## [Unreleased]\` → \`## [$SEMVER] - $(date +%Y-%m-%d)\` in \`CHANGELOG.md\`.
- Bumps \`$KUSTOMIZATION\` \`newTag\` from \`$CURRENT_TAG\` to \`$VERSION\`.

## Merge instructions

Either **squash-merge** or **"create a merge commit"** works.
\`tag-and-release.yml\` scans the merge commit message for a
\`release: $VERSION\` line — both merge modes emit one (squash puts it in
the subject, merge-commit puts it in the body), so either lands the tag.

After merge:
- Multi-arch image at \`ghcr.io/middlendian/fileblock-csi:$VERSION\` (and
  \`:latest\` for non-prereleases) is published.
- A GitHub release is created with the \`[$SEMVER]\` CHANGELOG section as
  its body, marked latest.
EOF
)"

git checkout main
echo
echo "Done. Branch \`$BRANCH\` pushed and PR opened."
echo "Merge it (squash or merge-commit, either is fine) — \`tag-and-release.yml\`"
echo "will detect the \`release: $VERSION\` line and publish the rest."
