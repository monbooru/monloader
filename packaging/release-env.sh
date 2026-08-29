VERSION=$(tr -d '[:space:]' < VERSION.md)
REPO=$(tr -d '[:space:]' < REPOSITORY.md)
DOC=$(tr -d '[:space:]' < DOC.md)
MOD="github.com/monbooru/monloader/internal/web"
LDFLAGS="-X '$MOD.Version=$VERSION' -X '$MOD.RepoURL=$REPO' -X '$MOD.DocURL=$DOC'"
