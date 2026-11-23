# Provision and mount a iSCSI volume

This example uses the StorageClass from the [manifest installation](../deploy/README.md).
It provisions a 1 GiB LUN and mounts it in a temporary pod. Run the commands
in one Bash session and stop if a command fails. On OpenShift you can use `oc` in place of `kubectl`.

## Create the claim

Confirm the cluster context and StorageClass. The provisioner should be
`iscsi.csi.ngxstorage.com`. The bundled class uses the `Delete` reclaim policy.

```bash
kubectl config current-context
kubectl get storageclass iscsi-csi-ngxstorage
ns=$(kubectl create -f - -o jsonpath='{.metadata.name}' <<'YAML'
apiVersion: v1
kind: Namespace
metadata:
  generateName: iscsi-volume-check-
YAML
)
```

```bash
kubectl -n "$ns" create -f - <<'YAML'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
spec:
  storageClassName: iscsi-csi-ngxstorage
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
YAML
```

## Mount the claim and check I/O

Use a BusyBox image available to your workers. If your cluster assigns a
UID or filesystem group, use the security context allowed for your workload.
OpenShift normally assigns these through its SCC. For Kubernetes namespaces
enforcing Restricted Pod Security, set `runAsNonRoot: true`, `runAsUser`
and `fsGroup` under `spec.securityContext` to permitted nonzero IDs. The
mounted directory must be writable by that user or group.

```bash
kubectl -n "$ns" create -f - <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: volume-check
spec:
  automountServiceAccountToken: false
  restartPolicy: Never
  securityContext:
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: client
      image: busybox:1.36.1
      command: ["sh", "-c", "sleep 3600"]
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: ["ALL"]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: data
YAML
kubectl -n "$ns" wait --for=condition=Ready pod/volume-check --timeout=3m
kubectl -n "$ns" get pvc data -o wide
kubectl -n "$ns" exec volume-check -- sh -ec '
  printf "%s\n" volume-check > /data/marker
  sync
  test "$(cat /data/marker)" = volume-check
  echo "Readback OK"
'
```

If the claim stays `Pending`, inspect `kubectl -n "$ns" describe pvc data`
for provisioning events and review the StorageClass parameters for your array.
If the claim is `Bound` and the pod is waiting, inspect
`kubectl -n "$ns" describe pod volume-check`. During attachment, check the portal group, worker initiator IQN, TCP port 3260 and active iSCSI sessions. An idle portal can refuse connections until a target is published. Successful ping alone does not verify login.

## Clean up

Capture the PV name before deleting the test claim. With the bundled class,
deleting the claim requests removal of its provisioned LUN. A class using
`Retain` leaves the PV and backing storage for the administrator.

```bash
pv=$(kubectl -n "$ns" get pvc data -o jsonpath='{.spec.volumeName}')
kubectl -n "$ns" delete pod volume-check --wait=true
kubectl -n "$ns" delete pvc data --wait=true
kubectl wait --for=delete "pv/$pv" --timeout=3m
kubectl delete namespace "$ns"
```
