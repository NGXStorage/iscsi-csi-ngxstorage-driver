#!/bin/sh

# The host integration files are needed only by the node operand. The
# controller uses the same image but intentionally has no /host mount.
role=node
expect_role=false
for arg in "$@"; do
    if [ "$expect_role" = true ]; then
        role=$arg
        expect_role=false
        continue
    fi
    case "$arg" in
        --role=*) role=${arg#--role=} ;;
        --role) expect_role=true ;;
    esac
done

if [ "$expect_role" = true ]; then
    echo "missing value for --role" >&2
    exit 2
fi

if [ "$role" != "controller" ]; then
    if [ ! -d /host ]; then
        echo "node role requires the host root mounted at /host" >&2
        exit 1
    fi
    if [ ! -d /host/etc/iscsi ]; then
        echo "node role requires host iSCSI configuration at /host/etc/iscsi" >&2
        exit 1
    fi
fi

exec /usr/local/bin/iscsi-csi-ngxstorage-driver "$@"
