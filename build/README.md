# iSCSI driver image build and push

Run from this driver repository. Requirements: Bash, Python 3, Git, Go, Docker
with Buildx and a running daemon. Builds target linux/amd64.

## Local build

```bash
TMPDIR="$HOME/.cache/ngxstorage" make docker-driver
```

The binary is compiled from the whole `./cmd/csi-driver` package. The build does
not run go mod tidy or rewrite go.mod/go.sum. The image version comes from
DriverVersion in pkg/driver/driver.go; this workflow does not cut a new release.

A clean build uses `<12-character-commit>`; dirty driver or local dependency
sources use `<12-character-commit>-dirty` and cannot be published. OCI revision
and vcs-ref contain the full driver commit. Additional labels and the ignored
`build/artifacts/image.json` record local dependency revisions and dirty state.
Neither a clean driver SHA nor a successful local build proves registry push.

The default local repository is `iscsi-csi-ngxstorage-driver`. `IMAGE=<repository>` selects
a different repository without changing the no-push behavior of the docker-driver target.
Use IMAGE without a tag, digest or URL scheme. No automatic branch/dev/latest
tag is created. The Docker context allowlist excludes .env, Git and test logs.

## Standalone build from vendored dependencies

The driver builds from its own vendor tree. The separate [CSI sanity runner](../docs/testing/sanity/GUIDE.md) uses a pinned csi-test fork shared by all three drivers.

The SDK, go-logutils and CSI utilities use pinned Go modules. The committed vendor tree
provides these dependencies for standalone builds.

```bash
CGO_ENABLED=0 GOFLAGS=-mod=vendor GOPROXY=off go build -trimpath \
  -o cmd/csi-driver/iscsi-csi-ngxstorage-driver ./cmd/csi-driver
```

`make build` also works from a standalone clone when `vendor/` is present
(Go selects vendor mode automatically). A vendor-mode binary is
code-identical to a module-cache build of the same sources but differs in
embedded Go build metadata (VCS stamp, module hashes); compare hashes only
between builds with matching flags.

The image workflow above (`build/image.py`) builds from the committed
`vendor/` tree when `vendor/modules.txt` is present (`-mod=vendor`, no sibling
checkouts or network needed); it falls back to module-cache mode only when no
vendor tree exists. It never rewrites go.mod/go.sum. Regenerate `vendor/` only
where the replace targets resolve (maintainer workspace): `go mod vendor`, then
verify go.mod/go.sum hashes are
unchanged.

## Publish a committed candidate

Commit the intended driver and local build-dependency changes first. Existing
unrelated dirty dependencies are not silently committed by this script. Use the
normal Docker credential store/helper or login interactively:

```bash
docker login quay.io
TMPDIR="$HOME/.cache/ngxstorage" make docker-push REGISTRY_TARGET=quay
```

This command **pushes** to `quay.io/ngxstorage/iscsi-csi-ngxstorage`. `REGISTRY_TARGET=prod` targets
`docker.io/ngxstorage/iscsi-csi-ngxstorage` and `REGISTRY_TARGET=test` targets `docker.io/babico/iscsi-csi-ngxstorage`;
all use commit tags, not branch names. Existing login environment variables
QUAY_LOGIN_NGX/DCKR_LOGIN_NGX/DCKR_LOGIN_BABICO remain supported via stdin.
.env is not loaded automatically and is not needed for local builds. Do not
pass a password on the command line or turn on shell tracing with credentials.

The image is built once. Before any push, every requested remote tag is checked:
missing tags may be created, identical image configs are reused, different
configs or multi-platform indexes are rejected. Authentication, network and TLS
errors stop publication; they are never interpreted as a missing tag. There is
no overwrite override. Configure registry-side tag immutability too when multiple
publishers share a repository; client checks alone are not an atomic registry lock.

For an explicitly authorized release, set both DRIVER_RELEASE and RELEASE_TAG:

```bash
DRIVER_RELEASE=1 RELEASE_TAG='<actual-driver-version>-1' \
  TMPDIR="$HOME/.cache/ngxstorage" make docker-push REGISTRY_TARGET=quay
```

The tag must match the compiled driver's version and release. It labels the same
image as the commit tag. DRIVER_VERSION and VCS_REF overrides, if supplied,
must equal the actual source version and full current commit.

After push, each tag is read back and its verified registry digest is recorded
in `build/artifacts/image.json`. A partial failure leaves only verified completed
references in `published`; inspect registry state before retrying. Do not rebuild
or overwrite an existing commit tag to hide a mismatch.

## Select a deployment artifact

Use the verified digest from `published` for [manual deployment](../deploy/README.md).
For Helm, image.tag identifies the matching source commit and image.digest pins
that exact artifact; digest takes precedence. Never substitute latest/dev or
assume a digest belongs to a tag without checking its OCI revision. The existing
Helm workflow is reviewed separately; building does not update chart defaults,
publish a chart, or change the cluster.

```bash
python3 build/test_image.py
```

This regression check needs no registry write and covers missing/auth/network
responses, immutable tag collisions and source provenance validation. Real local
image build and live registry inspection remain separate evidence.
