# Publishing Docker image

The `bisonwallet-test` image referenced by `docker-compose.yml` is built and published to the GitHub Container Registry (`ghcr.io/<repo-owner>/bisonwallet-test`) by the `.github/workflows/release-docker.yml` workflow, which runs when a release is published or on manual dispatch (the `tag` input sets the image tag, e.g. `v1.1.0-rc3`).

The workflow prints the published image reference with its sha256 manifest digest at the end of the build step; copy it into the `image:` line of `docker-compose.yml` to pin the umbrel config to the exact image that was built.

Note that the first push creates the GHCR package as **private**; it must be switched to public once (GitHub profile → Packages → `bisonwallet-test` → Package settings → Change visibility) or umbrelOS will not be able to pull it.

Alternatively, the image can be built and published manually using the following steps.  This requires the use of [BuildKit](https://docs.docker.com/build/buildkit/), which is part of recent Docker releases.

1. Log in to GHCR with a personal access token that has the `write:packages` scope

```bash
docker login ghcr.io
```

1. Build the image

```bash
git clone https://github.com/decred/dcrdex
cd dcrdex
git checkout release-v1.x.x
docker buildx create --use
docker buildx build -f client/Dockerfile.release \
  --platform linux/arm64,linux/amd64 \
  --tag ghcr.io/<repo-owner>/bisonwallet-test:v1.x.x \
  --output "type=registry"  .
```

This is a multi-platform (targeting `amd64` and `arm64`) build which takes longer, this is normal.
If there are no error messages, at the end of the build the image will be published to GHCR.

1. Verify that the image has been published under the owner's GitHub packages (`https://github.com/<repo-owner>?tab=packages`).  There should be 2 digest lines; these indicate that both target platforms have been built and are included in the published image.  After publishing manually, update the `image:` digest pin in `docker-compose.yml` to match.
