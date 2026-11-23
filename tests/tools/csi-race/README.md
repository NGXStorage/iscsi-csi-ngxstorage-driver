# csi-race

`csi-race` is a **test-only** direct CSI controller concurrency helper for Plan
11.1. It connects to a Unix CSI socket (normally `/csi/csi.sock`) and releases
all requested RPC goroutines through the same start barrier.

It is intentionally not copied into FC/iSCSI/NFS production images. Build it
for a test sidecar/pod that shares the controller pod's `/csi` volume.

```sh
go build -o "$HOME/.cache/ngxstorage/ngxstorage-csi-csi-race" .

$HOME/.cache/ngxstorage/ngxstorage-csi-csi-race create-volume \
  --endpoint unix:///csi/csi.sock \
  --name race-volume --count 2 --capacity-bytes 1073741824

$HOME/.cache/ngxstorage/ngxstorage-csi-csi-race controller-expand \
  --endpoint unix:///csi/csi.sock \
  --volume-id '<csi-volume-id>' \
  --sizes 2147483648,3221225472

$HOME/.cache/ngxstorage/ngxstorage-csi-csi-race create-snapshot \
  --endpoint unix:///csi/csi.sock \
  --source-volume-id '<csi-volume-id>' \
  --name race-snapshot --count 2
```

The helper exits non-zero when an RPC fails. Same-name CreateVolume and
CreateSnapshot additionally require every successful response to return the
same non-empty object ID. `controller-expand` performs a final
`ControllerGetVolume` readback and requires the final capacity to equal the
largest requested size exactly, so a stale double-add against an additive
backend is detected.

`delete-volume`, `delete-snapshot`, and `controller-unpublish` exist only to
support deterministic scenario cleanup/orchestration. `tests/scenario/lib.sh`
temporarily injects this binary into the live controller pod's writable `/csi`
emptyDir and removes it during teardown; production driver images remain
unchanged.
