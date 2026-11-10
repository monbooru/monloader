# Contributing

See [monbooru's CONTRIBUTING](https://github.com/monbooru/monbooru/blob/main/CONTRIBUTING.md);
it covers all the monbooru repos, including this one.

Code changes come as pull requests against `dev`; `main` holds one snapshot per release, without the tests, and is the default branch, so check the base branch. Verify runs on every pull request: the build, the cross-compiles, the advisories check and the linter; it fails a pull request that targets `main`. The Go tests, with the race detector and an 80% coverage floor, run when a maintainer comments `/test` on the pull request. The site table has to match the pinned gallery-dl, and a site profile goes in as its file under `internal/mapping/profiles/`. The `dev-build` pre-release and the `:dev` image tag are builds of `dev` for testing, not releases.

Open issues for downloading and site support here.
