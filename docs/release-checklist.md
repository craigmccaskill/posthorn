# Release checklist (v1.0.0)

Tag-day procedure for the v1.0.0 release. Follow in order. The release workflow at [`.github/workflows/release.yml`](../.github/workflows/release.yml) takes over once the tag is pushed; everything before that is preparation you do locally.

## 0. Operator validation (gating)

These three acceptance tests gate the tag. **Don't proceed if any fails.**

### Story 5.1 — Docker smoke test

```bash
docker pull ghcr.io/craigmccaskill/posthorn:0.0.1-test   # the test image we already built
mkdir -p /tmp/posthorn-smoke && cd /tmp/posthorn-smoke
cat > posthorn.toml <<'EOF'
[[endpoints]]
path = "/api/contact"
to = ["you@yourdomain.com"]
from = "Posthorn Test <noreply@yourdomain.com>"
required = ["name", "email", "message"]
subject = "Smoke: {{.name}}"
body = "From {{.name}} <{{.email}}>: {{.message}}"

[endpoints.transport]
type = "postmark"

[endpoints.transport.settings]
api_key = "${env.POSTMARK_API_KEY}"
EOF

docker run --rm -d -p 8080:8080 \
  -v $PWD/posthorn.toml:/etc/posthorn/config.toml:ro \
  -e POSTMARK_API_KEY=$POSTMARK_API_KEY \
  --name posthorn-smoke ghcr.io/craigmccaskill/posthorn:0.0.1-test
sleep 1

curl -i -X POST http://localhost:8080/api/contact \
  -d "name=Smoke" -d "email=test@example.com" -d "message=hello"

docker logs posthorn-smoke   # confirm structured JSON logs
docker stop posthorn-smoke
```

**Pass criteria:** HTTP 200 with submission_id, email arrives in your inbox.

### Story 6.1 — `xcaddy build` produces a working binary

```bash
cd <repo-root>/caddy
xcaddy build \
  --with github.com/craigmccaskill/posthorn/caddy=. \
  --with github.com/craigmccaskill/posthorn=../core
./caddy list-modules | grep posthorn
```

**Pass criteria:** binary produced, `caddy list-modules` includes `http.handlers.posthorn`.

### Story 6.3 — Manual parity test

Full procedure: [docs/manual-test.md](./manual-test.md). Both deployment shapes deliver identical mail for identical input.

## 1. Generate `caddy/go.sum`

CI currently re-downloads caddy deps every run because we never committed `go.sum`. Fix it once now so the release builds against a pinned dep tree:

```bash
cd <repo-root>/caddy
go mod tidy
git add go.sum
git commit -m "chore(caddy): commit go.sum"
```

## 2. Update `caddy/go.mod` for external installability

The current `caddy/go.mod` has a dev-only `replace` directive pointing to `../core`. That works for in-workspace builds but breaks `xcaddy build --with github.com/craigmccaskill/posthorn/caddy@v1.0.0` for external users (they don't have a local `../core`). Replace it with a real version pin once you've tagged core:

After step 5 below (tagging the root), come back and edit `caddy/go.mod`:

```diff
-// Pre-v1.0 development: resolve the core module from the local checkout.
-// This `replace` directive comes out as part of the v1.0.0 release prep
-// (Story 7.3) so external `xcaddy build` invocations resolve core via
-// the published module proxy.
-replace github.com/craigmccaskill/posthorn => ../core
-
 require (
   github.com/caddyserver/caddy/v2 v2.10.0
-  github.com/craigmccaskill/posthorn v0.0.0-00010101000000-000000000000
+  github.com/craigmccaskill/posthorn v1.0.0
   go.uber.org/zap v1.27.0
   go.uber.org/zap/exp v0.3.0
 )
```

Then `go mod tidy` again to regenerate `go.sum` against the published core. Commit:

```bash
cd <repo-root>/caddy
go mod tidy
git add go.mod go.sum
git commit -m "chore(caddy): pin core to v1.0.0 (post-tag)"
```

## 3. Update CHANGELOG date if needed

[`CHANGELOG.md`](../CHANGELOG.md) currently has `## [1.0.0] — 2026-05-16` as a placeholder. Update if the actual tag day differs.

## 4. Verify CI on main is green

```bash
gh run list --workflow=ci.yml --branch=main --limit=1
```

Must be ✅ before tagging.

## 5. Tag and push `v1.0.0` (core)

```bash
git tag -a v1.0.0 -m "Posthorn v1.0.0 — first public release

HTTP form ingress, Postmark transport, standalone Docker + optional
Caddy adapter. Full release notes in CHANGELOG.md."

git push origin v1.0.0
```

This fires [`.github/workflows/release.yml`](../.github/workflows/release.yml). Watch:

```bash
gh run watch $(gh run list --workflow=release.yml --limit=1 --json databaseId -q '.[0].databaseId')
```

The workflow:
1. Builds `linux/amd64` + `linux/arm64` via buildx
2. Tags as `:1.0.0`, `:1.0`, `:1`, `:latest` (because no pre-release suffix)
3. Pushes to `ghcr.io/craigmccaskill/posthorn`

**Pass criteria:** workflow green, `docker pull ghcr.io/craigmccaskill/posthorn:1.0.0` works on both archs, `docker pull ghcr.io/craigmccaskill/posthorn:latest` resolves to the same digest.

## 6. Do step 2 (caddy/go.mod swap), then tag `caddy/v1.0.0`

The caddy submodule needs its own version tag for `xcaddy build --with github.com/craigmccaskill/posthorn/caddy@v1.0.0` to resolve via the module proxy. Go's convention for tagged submodules within a single repo is `<subdir>/<version>`:

```bash
git tag -a caddy/v1.0.0 -m "Caddy adapter v1.0.0"
git push origin caddy/v1.0.0
```

Verify after a minute or two:

```bash
go list -m github.com/craigmccaskill/posthorn/caddy@v1.0.0
# expect: github.com/craigmccaskill/posthorn/caddy v1.0.0
```

## 7. Create the GitHub Release

```bash
gh release create v1.0.0 \
  --title "v1.0.0 — first public release" \
  --notes-file <(awk '/^## \[1.0.0\]/,/^## \[/{print}' CHANGELOG.md | head -n -1)
```

Or use the GitHub UI to attach the same notes.

## 8. File the Caddy modules-page submission

Per R3 in the project brief — discoverability for the adapter. Within 7 days of the tag:

```bash
# Fork caddyserver/website if you haven't already, then:
gh repo clone craigmccaskill/website
cd website
# Add an entry to the modules listing (check the current data layout
# at caddyserver/website — past PRs are a good template)
# Open a PR pointing at github.com/craigmccaskill/posthorn/caddy
```

The acceptance criterion is "PR submitted within 7 days," not "PR merged" — landing is up to the Caddy team's review cadence.

## 9. Optional cleanup

```bash
# Delete the v0.0.1-test git tag if you don't want it kept as history
git push origin :refs/tags/v0.0.1-test
git tag -d v0.0.1-test

# Delete the stale GHCR package version (needs delete:packages scope)
gh auth refresh -s read:packages,delete:packages
VERSION_ID=$(gh api /users/craigmccaskill/packages/container/posthorn/versions \
  --jq '.[] | select(.metadata.container.tags[]? == "0.0.1-test") | .id')
gh api --method DELETE /users/craigmccaskill/packages/container/posthorn/versions/$VERSION_ID
```

## 10. Update CLAUDE.md status pointer

Move "Current task" to: "v1.0.0 shipped on 2026-05-XX. Open: v1.1 planning."

## 11. Tell the world

- HN Show post linking to posthorn.dev (per R3 mitigation — discoverability)
- Personal blog launch post (already planned per the brief — DigitalOcean SMTP-block story as the narrative hook)
- Add a "Show off" entry in the project Obsidian dashboard

Done. v1.0.0 shipped.
