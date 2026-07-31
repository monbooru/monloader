# Stage 1: build the static Go binary.
FROM golang:1.25-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN VERSION=$(cat VERSION.md 2>/dev/null | tr -d '[:space:]') && \
    REPO=$(cat REPOSITORY.md 2>/dev/null | tr -d '[:space:]') && \
    DOC=$(cat DOC.md 2>/dev/null | tr -d '[:space:]') && \
    CGO_ENABLED=0 go build \
      -ldflags="-s -w -X 'github.com/monbooru/monloader/internal/web.Version=${VERSION}' -X 'github.com/monbooru/monloader/internal/web.RepoURL=${REPO}' -X 'github.com/monbooru/monloader/internal/web.DocURL=${DOC}'" \
      -o /monloader ./cmd/monloader

# Stage 2: runtime. Needs Python for the gallery-dl subprocess. The generated
# supportedsites.md is bundled at the same pinned version (the app seeds site names and auth kinds from it)
FROM python:3.13-slim-trixie
RUN pip install --no-cache-dir gallery-dl==1.32.8 && \
    mkdir -p /usr/local/share/monloader && \
    python -c "import gallery_dl.version, urllib.request; urllib.request.urlretrieve('https://raw.githubusercontent.com/mikf/gallery-dl/v' + gallery_dl.version.__version__ + '/docs/supportedsites.md', '/usr/local/share/monloader/supportedsites.md')"

COPY --from=build /monloader /usr/local/bin/monloader

RUN useradd --uid 1000 --user-group --create-home --home-dir /home/monloader --shell /usr/sbin/nologin monloader
USER 1000:1000
ENV HOME=/home/monloader \
    MONLOADER_SERVER_BIND_ADDRESS=0.0.0.0:8081

EXPOSE 8081
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/monloader", "healthcheck", "-config", "/config/monloader.toml"]
ENTRYPOINT ["/usr/local/bin/monloader", "-config", "/config/monloader.toml"]
