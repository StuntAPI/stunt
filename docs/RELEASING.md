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

## 3. Homebrew tap

Create a public repo **`stuntapi/homebrew-tap`** (empty — GoReleaser writes
`Formula/stunt.rb` into it on each release).

Then `brew install stuntapi/tap/stunt` works.

---

## 4. CI / Release secrets

In `stuntapi/stunt` → **Settings → Secrets and variables → Actions**, add:

| Secret | Purpose |
|---|---|
| `HOMEBREW_TAP_GITHUB_TOKEN` | Fine-grained PAT with `contents: write` on `stuntapi/homebrew-tap`. GoReleaser uses it to push the formula. |

`GITHUB_TOKEN` is provided automatically (used to create the Release).

---

## 5. Cut a release

```sh
git tag v0.1.0
git push --tags
```

The **Release** workflow (`.github/workflows/release.yml`) then:

1. Runs `just ci` (never releases a broken build).
2. GoReleaser builds `linux/darwin/windows × amd64/arm64`, archives, checksums, SBOMs.
3. Publishes a **draft** GitHub Release (review then publish).
4. Pushes `Formula/stunt.rb` → `stuntapi/homebrew-tap`.

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
- [ ] **Move repo** into `stuntapi` org, make public
- [ ] **Host vanity page** at `stuntapi.com/stunt` (GitHub Pages)
- [ ] **Create** `stuntapi/homebrew-tap` repo
- [ ] **Add** `HOMEBREW_TAP_GITHUB_TOKEN` secret
- [ ] **Tag** `v0.1.0` and push
