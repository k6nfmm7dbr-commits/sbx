```bash
repo=k6nfmm7dbr-commits/sbx
rev=$(git ls-remote "https://github.com/$repo.git" refs/heads/dist | cut -f1)
[[ "$rev" =~ ^[0-9a-f]{40}$ ]] &&
  base="https://raw.githubusercontent.com/$repo/$rev" &&
  curl -fsSLO "$base/sbx.sh" &&
  curl -fsSLO "$base/sbx.sh.sha256" &&
  sha256sum -c sbx.sh.sha256 &&
  sudo env SBX_DIST_SHA="$rev" bash sbx.sh
```
