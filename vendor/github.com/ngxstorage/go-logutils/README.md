# NGX Storage go-logutils

Shared Zap logging for NGX Storage Go services (the FC, iSCSI and NFS CSI
drivers) and the NGX Storage Go SDK.

- stdout + file dual output; stdout only when no directory is configured
- 24h rotation (`<prefix>-<node>-<UTC>.log` → `.log.gz`), 30-day retention
- prefix-scoped globbing: one installation never rotates or deletes another's files
- directory `0750`, files `0640`
- one `Instance` per process; no package-global logger
- SDK adapter (`NewSDKLogger`) so drivers can plug the logger into the Go SDK

## Install

```bash
go get github.com/ngxstorage/go-logutils
```

## Usage

Node deployments set `NGXSTORAGE_CSI_LOG_DIR` to the protocol-specific
`/host/var/log/ngxstorage/<protocol>` directory:

```go
instance, err := logutils.NewFromEnvironment("info", nodeID, "nfs", driverNamePrefix)
if err != nil {
    return err
}
defer instance.Close()

csiDriver, err := driver.NewDriver(driver.InputParameters{
    Logger: instance.Logger,
})
```

With an empty `driverNamePrefix`, node files start with `nfs-`. A prefix such as
`201` produces `nfs-201-...log`, so rotation and retention from another
installation (for example `205`) cannot select those files. Controllers do not
set the directory environment variable and therefore remain stdout-only.

## API

| Symbol | Purpose |
|--------|---------|
| `New(Config)` | build an `Instance` from explicit configuration |
| `NewFromEnvironment(consoleLevel, nodeID, protocol, driverNamePrefix)` | build from `NGXSTORAGE_CSI_LOG_DIR` |
| `Instance.Logger` | `*zap.Logger` handed to the driver/SDK |
| `Instance.Close()` | idempotent; stops and joins maintenance before closing the file |
| `ParseLevel(string)` | parse a console level |
| `ScopedPrefix(protocol, prefix)` | file prefix for one instance |
| `NewSDKLogger(*zap.Logger)` | adapter implementing the SDK logger interface |

`Config` fields: `Directory`, `FilePrefix`, `NodeID`, `ConsoleLevel`,
`RetentionDays` (0 → 30), `Interval` (0 → safe default).

An empty `Directory` keeps logging on stdout and creates no temporary file.

## Test

```bash
go test ./...
go test -race ./...
go vet ./...
```

## License

Apache-2.0
