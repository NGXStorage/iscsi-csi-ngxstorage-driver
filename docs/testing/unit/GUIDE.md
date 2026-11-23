# Unit test guide

Unit tests use in-process fakes and do not prove behavior against a live backend or cluster. From the driver root:

```bash
TMPDIR="$HOME/.cache/ngxstorage" make test
```

For focused work use `TMPDIR="$HOME/.cache/ngxstorage" go test ./pkg/driver` or the relevant protocol package. Keep Go caches under `$HOME/.cache/ngxstorage`.

Save run results outside the repository with the source/image identity, command, outcome and cleanup evidence.
