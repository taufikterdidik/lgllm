# Release checklist

A release is a tag push, not a manual upload. The tag starts
`.github/workflows/release.yml`, which builds the artifacts, image and GitHub
release from the same commit.

## Versioning

- SemVer: `vMAJOR.MINOR.PATCH`.
- Release candidates use a suffix: `v0.2.0-rc.1`; they are marked as
  prereleases and do not move `latest`.
- Use annotated tags only. Never move or delete a published tag.

## Pre-flight

From a clean `main`:

```bash
git pull --ff-only
make check
go test -race ./...
make snapshot                  # requires goreleaser
docker build -t lgllm:check .  # optional local container check
```

Then verify the snapshot:

```bash
dist/lgllm_*_linux_amd64_v1/lgllm -version
tar tzf dist/lgllm_*_linux_amd64.tar.gz | grep -E 'LICENSE|lgllm|config.example'
dpkg-deb -c dist/lgllm_*_linux_amd64.deb | grep -E 'usr/bin/lgllm|lgllm.service'
```

Release notes must call out breaking changes, new config fields, and upgrades.
Strict config parsing makes removed or renamed fields a breaking change.

## Publish

```bash
git tag -a v0.1.0 -m "lgllm v0.1.0"
git push origin main
git push origin v0.1.0
```

The workflow runs these jobs in order:

1. **test** — formatting, vet, tests and race tests.
2. **goreleaser** — archives, deb/rpm/apk, checksums, draft release.
3. **image** — multi-arch GHCR image, SBOM/provenance, cosign signature.
4. **publish** — turns the draft into a stable release or prerelease.

If any job fails, fix the cause and move the tag only if it was not published
yet. Do not publish a partial release.

The first push creates a new GHCR package. Set its visibility to public once in
the repository's package settings if anonymous pulls are intended; later tags
reuse the same package.

## Post-release verification

- Download one archive and `checksums.txt`; verify the checksum.
- Run `lgllm -version` and confirm the tag, commit and date.
- Run `lgllm -config config.example.yaml -check`.
- Pull the image and confirm the digest matches the signed digest.
- Run the image with a read-only config and check `/healthz`.
- Install the package on a clean Debian/Fedora/Alpine host and start the
  systemd unit.
- Keep the previous image digest for rollback.

## Rollback

There is no destructive migration in the current release line. To roll back:

- install the previous package or binary;
- restore the previous image digest;
- keep the config file, because all current fields are additive.

If a release has a broken config field, publish a patch release rather than
silently changing the tag.
