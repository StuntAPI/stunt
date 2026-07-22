# Release & Distribution Setup

This document covers the one-time setup to make `stunt` installable via
`go install`, Homebrew, and the GitHub Releases page, plus the recurring
"how to cut a release" steps.

---

## 1. Module path & vanity import

The Go module path is **`stuntapi.com/stunt`** (a vanity import), which decouples
the permanent module identity from the GitHub repo location. For `go install` /
`go get` to resolve it, the domain must serve a `go-import` meta tag.

### Host the vanity redirect

The page is already authored at [`.vanity/index.html`](.vanity/index.html).
Host it so that `https://stuntapi.com/stunt` serves it (GitHub Pages recommended):

**GitHub Pages (free):**
1. Create a repo `stuntapi/stuntapi.github.io` (or use a `docs/` site / `gh-pages` branch).
2. Put the contents of `.vanity/index.html` at the path `stunt/index.html`
   (so the URL `https://stuntapi.github.io/stunt` serves it).
3. Point **stuntapi.com** DNS at GitHub Pages:
   - Add an `A` record (apex) or `CNAME` (subdomain) per
     [GitHub's IPs](https://docs.github.com/en/pages/configuring-a-custom-domain-for-your-github-pages-site).
   - In the repo **Settings → Pages → Custom domain**, enter `stuntapi.com`.
4. Enable **Enforce HTTPS**.

**Cloudflare Pages / Netlify (alternative):** connect the `stuntapi.com` domain,
deploy the single static file at path `/stunt`.

### Verify

```sh
curl -s "https://stuntapi.com/stunt?go-get=1" | grep go-import
# expect: <meta name="go-import" content="stuntapi.com/stunt git https://github.com/stuntapi/stunt">
```

Then (once the repo is public):

```sh
go install stuntapi.com/stunt/cmd/stunt@latest
```

---

## 2. Move the repo into the org

```sh
# In stuntapi/stunt repo settings → Transfer, or:
git remote set-url origin git@github.com:stuntapi/stunt.git
git push -u origin main
```

Make the repo **public** (Settings → General → Danger Zone).

---

## 3. Packaging repos

Create two **public** repos (empty — GoReleaser writes into them on each release):

- **`stuntapi/homebrew-tap`** — receives `Casks/stunt.rb` (Homebrew Cask, macOS).
- **`stuntapi/winget`** — receives the `StuntAPI.Stunt` winget manifest (Windows).

Then `brew install --cask stuntapi/tap/stunt` (macOS) and
`winget install --manifest https://github.com/stuntapi/winget` (Windows) work.

> **Why Casks, not Formulas?** GoReleaser removed `brews` (Formulas) in v2.16 in
> favour of Casks. Casks are macOS-only, so non-macOS users install via
> `go install` (works everywhere) or the release archive. This is the standard
> Go-CLI distribution pattern.

---

## 4. CI / Release secrets

In `stuntapi/stunt` → **Settings → Secrets and variables → Actions**, add:

| Secret | Purpose |
|---|---|
| `TAP_GITHUB_TOKEN` | Fine-grained PAT with `contents: write` on **both** `stuntapi/homebrew-tap` **and** `stuntapi/winget`. GoReleaser uses it to push the Cask + winget manifest. |

`GITHUB_TOKEN` is provided automatically (used to create the Release).

---

## 5. Cut a release

There are **two** paths — both produce the same artifacts.

### Path A — GitHub Actions (default)

```sh
git tag v0.1.0
git push --tags
```

The **Release** workflow (`.github/workflows/release.yml`) then:

1. Runs `just ci` (never releases a broken build).
2. GoReleaser builds `linux/darwin/windows × amd64/arm64`, archives, checksums, SBOMs.
3. Publishes a **draft** GitHub Release (review then publish).
4. Pushes `Casks/stunt.rb` → `stuntapi/homebrew-tap` and the winget manifest → `stuntapi/winget`.

### Path B — local release (no GitHub Actions dependency)

Use this when Actions can't run — billing exhausted, a GitHub outage, or a
fresh machine. It cuts the identical release from your laptop:

```sh
export TAP_GITHUB_TOKEN=<PAT: contents:write on stuntapi/homebrew-tap + stuntapi/winget>
export GITHUB_TOKEN="$(gh auth token)"

git tag v0.x.y          # tag at HEAD (GoReleaser derives the version from it)
just release            # runs the local CI gate, then goreleaser
# just release --no-ci  # skip the gate — release even if ci cannot run
```

`just release` ensures `syft` is installed (the `sboms` pipe needs it),
gates on `just ci` by default, and runs `goreleaser release --clean` with the
tokens from your environment. Use **one** path per release — don't run both.

Release is marked `prerelease: auto` and `draft: true` — review the draft, then
publish it.

---

## Summary of one-time owner tasks

- [x] Register `stuntapi.com`
- [x] Secure `stuntapi` GitHub account
- [x] Apache 2.0 LICENSE / NOTICE / TRADEMARKS.md
- [x] Migrate module path → `stuntapi.com/stunt`
- [x] `.goreleaser.yaml` (validated, snapshot build passes)
- [x] CI + Release workflows
- [x] Vanity redirect page (`.vanity/index.html`)
- [x] **Move repo** into `stuntapi` org, make public
- [x] **Host vanity page** at `stuntapi.com/stunt` (served by stuntapi.com)
- [x] **Create** `stuntapi/homebrew-tap` repo
- [x] **Create** `stuntapi/winget` repo
- [x] **Wire** GoReleaser `homebrew_casks` + `winget` (migrated off removed `brews`)
- [ ] **Add** `TAP_GITHUB_TOKEN` secret (PAT: `contents:write` on homebrew-tap + winget)
- [ ] **Tag** `v0.1.0` and push
