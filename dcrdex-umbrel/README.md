# Publishing Docker image

The `decred/bisonwallet` image referenced by `docker-compose.yml` is normally built and published by the `.github/workflows/release-docker.yml` workflow, which runs when a release is published or on manual dispatch (the `tag` input sets the image tag, e.g. `v1.1.0-rc3`).

Alternatively, it can be built and published to Docker Hub manually using the following steps.  This requires the use of [BuildKit](https://docs.docker.com/build/buildkit/), which is part of recent Docker releases.

1. Log in to Docker Hub using the credentials that have write access to <https://hub.docker.com/u/decred>

```bash
docker login
```

1. Build the image

```bash
git clone https://github.com/decred/dcrdex
cd dcrdex
git checkout release-v1.x.x
docker buildx create --use
docker buildx build -f client/Dockerfile.release \
  --platform linux/arm64,linux/amd64 \
  --tag decred/bisonwallet:v1.x.x \
  --output "type=registry"  .
```

This is a multi-platform (targeting `amd64` and `arm64`) build which takes longer, this is normal.
If there are no error messages, at the end of the build the image will be published to Docker.

1. Verify that the image has been published on <https://hub.docker.com/r/decred/bisonwallet/tags>.  There should be 2 digest lines; these indicate that both target platforms have been built and are included in the published image.  After publishing, update the `image:` digest pin in `docker-compose.yml` to match.
