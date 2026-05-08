#!/usr/bin/env bash
#
# hack/cut-release.sh vX.Y.Z[-prerelease]
#
# Local helper to cut a release. Assumes the maintainer has already edited
# CHANGELOG.md to promote the current `## [Unreleased]` section to
# `## [X.Y.Z] - YYYY-MM-DD` (and added a fresh empty `## [Unreleased]`
# above it, plus the `[X.Y.Z]` link reference at the bottom).
#
# What this script does:
#   1. Bumps `deploy/kustomize/base/kustomization.yaml` newTag from
#      `latest` to vX.Y.Z so a checkout at the tag installs the matching
#      image.
#   2. Commits the (CHANGELOG + kustomization) bump as "release: vX.Y.Z"
#      and tags that commit.
#   3. Bumps newTag back to `latest` in a follow-up commit so `main`
#      continues to track latest.
#   4. Prints the push command — does NOT push.
#
# Re-run safely: if anything fails partway, `git reset --hard origin/main`
# will undo the local commits, and `git tag -d vX.Y.Z` removes the tag.
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

# Branch + sync checks.
BRANCH="$(git rev-parse --abbrev-ref HEAD)"
[ "$BRANCH" = "main" ] || err "must be on 'main' (currently on '$BRANCH')"
git fetch origin main --quiet
[ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ] \
  || err "local 'main' is not at origin/main — pull/rebase first"

# Tag must not already exist locally or on origin.
git rev-parse "$VERSION" >/dev/null 2>&1 \
  && err "tag $VERSION already exists locally"
git ls-remote --exit-code --tags origin "$VERSION" >/dev/null 2>&1 \
  && err "tag $VERSION already exists on origin"

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

# Kustomization base must currently be at newTag: latest (single match).
LATEST_LINES="$(grep -cE '^[[:space:]]+newTag: latest$' "$KUSTOMIZATION" || true)"
[ "$LATEST_LINES" = "1" ] \
  || err "$KUSTOMIZATION does not have exactly one 'newTag: latest' line (found: $LATEST_LINES)"

echo "==> Bumping $KUSTOMIZATION newTag: latest -> $VERSION"
sed -i -E "s|^([[:space:]]+)newTag: latest$|\\1newTag: $VERSION|" "$KUSTOMIZATION"

echo "==> Committing release: $VERSION"
git add CHANGELOG.md "$KUSTOMIZATION"
git commit -m "release: $VERSION"

echo "==> Tagging $VERSION"
git tag -a "$VERSION" -m "$VERSION"

echo "==> Reverting $KUSTOMIZATION newTag: $VERSION -> latest"
sed -i -E "s|^([[:space:]]+)newTag: $VERSION\$|\\1newTag: latest|" "$KUSTOMIZATION"

echo "==> Committing back-to-latest"
git add "$KUSTOMIZATION"
git commit -m "deploy: bump kustomization back to :latest after $VERSION"

echo
echo "Done. Recent commits:"
git log --oneline -3
echo
echo "Tag:"
git show --no-patch --format="  %h %s%n  tag $VERSION%n" "$VERSION"
echo "If everything looks right, push with:"
echo "  git push origin main $VERSION"
