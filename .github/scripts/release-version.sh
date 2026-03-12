#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "usage: $0 <git-tag> [--write-python]" >&2
  exit 1
fi

tag="$1"
shift

write_python=false
for arg in "$@"; do
  case "$arg" in
    --write-python)
      write_python=true
      ;;
    *)
      echo "unknown argument: $arg" >&2
      exit 1
      ;;
  esac
done

release_tag="${tag}"
release_version="${tag#v}"
helm_version="${release_version}"
pypi_version="${release_version}"

if [[ "${release_version}" == *-* ]]; then
  if [[ "${release_version}" =~ ^([0-9]+\.[0-9]+\.[0-9]+)-alpha\.([0-9]+)$ ]]; then
    pypi_version="${BASH_REMATCH[1]}a${BASH_REMATCH[2]}"
  elif [[ "${release_version}" =~ ^([0-9]+\.[0-9]+\.[0-9]+)-beta\.([0-9]+)$ ]]; then
    pypi_version="${BASH_REMATCH[1]}b${BASH_REMATCH[2]}"
  elif [[ "${release_version}" =~ ^([0-9]+\.[0-9]+\.[0-9]+)-rc\.([0-9]+)$ ]]; then
    pypi_version="${BASH_REMATCH[1]}rc${BASH_REMATCH[2]}"
  else
    echo "unsupported prerelease tag format: ${tag}" >&2
    exit 1
  fi
fi

if [[ "${write_python}" == "true" ]]; then
  repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
  cat > "${repo_root}/sdk/python/src/capsule_sdk/_version.py" <<EOF
__version__ = "${pypi_version}"
EOF
fi

cat <<EOF
RELEASE_TAG='${release_tag}'
RELEASE_VERSION='${release_version}'
HELM_VERSION='${helm_version}'
PYPI_VERSION='${pypi_version}'
EOF
