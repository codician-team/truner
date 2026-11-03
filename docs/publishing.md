# Publishing truner (maintainers)

This repository refers to itself as `truner/truner` (module path, release download
URLs, install scripts, docs and the website) and to its website as
`truner.github.io/truner`. To publish under another GitHub owner, for example
`your-name/truner`:

```bash
git grep -l -e 'truner/truner' -e 'truner\.github\.io' \
  | xargs sed -i -e 's#truner/truner#your-name/truner#g' -e 's#truner\.github\.io#your-name.github.io#g'
go mod tidy && go build ./... && go test ./...
git commit -am "Publish as your-name/truner"
```

Then create the repository on GitHub and push:

```bash
git remote add origin git@github.com:your-name/truner.git
git push -u origin main
```

## Cutting a release

1. Add a `## vX.Y.Z` section to `CHANGELOG.md`.
2. Tag and push:

   ```bash
   git tag v0.1.0
   git push origin v0.1.0
   ```

3. The `Release` workflow tests, builds all binaries (CLI for Linux/macOS/Windows on
   amd64/arm64 with the agent and Lambda embedded, plus the standalone artifacts) and
   publishes a GitHub release with `checksums.txt`. The build passes
   `-repo $GITHUB_REPOSITORY`, so CLIs built from forks download from their own releases.

`install.sh` and `install.ps1` download from the latest release of the repository
they name (or `TRUNER_REPO`).

## Documentation website

The `Docs` workflow builds the site in `docs/` with VitePress and deploys it to GitHub
Pages on every push to `main` that touches the docs. Enable it once: **Settings → Pages
→ Build and deployment → Source: GitHub Actions**. The site is then served at
`https://<owner>.github.io/<repository>/`. With a custom domain it is served from the
root, and the workflow adjusts the base path on its own. While the repository is
private, the workflow only checks that the site builds.

Preview locally:

```bash
cd docs
npm ci
npm run dev
```

## Local release build

```bash
go run ./tools/build -version v0.1.0 -repo your-name/truner -out dist
```
