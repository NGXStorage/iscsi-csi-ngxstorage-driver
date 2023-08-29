# Use an existing volume

Below you will find the instruction on how to use an existing NGX Storage Block Storage LUN with your Kubernetes cluster.

## Preconditions

Make sure the LUN to be imported is not attached anywhere yet and make sure that the existing LUN is in the Pool specified when starting the driver. For a volume that is still attached to a Kubernetes cluster, you need to ensure the reclaim policy on the associated PersistentVolume is set to _`Retain`_. Afterwards, the workload / PersistentVolumeClaim can be deleted, which will cause the backing volume to be detached without deletion.

## Example

To use an existing volume, we have to create manually a `PersistentVolume` (PV) resource. Here is an example `PersistentVolume` resource for an existing volume (change `ALREADYCREATEDLUNNAME` to your existing LUN name)):

```yaml
kind: PersistentVolume
apiVersion: v1
metadata:
  name: volume-pv-01
  annotations:
    pv.kubernetes.io/provisioned-by: iscsi.csi.ngxstorage.com
spec:
  storageClassName: iscsi-csi-ngxstorage-class
  persistentVolumeReclaimPolicy: Delete
  capacity:
    storage: 5Gi
  accessModes:
  - ReadWriteOnce
  csi:
    driver: iscsi.csi.ngxstorage.com
    volumeHandle: iscsi.ngxstorage.ALREADYCREATEDLUNNAME
    fsType: noformat
```

Couple of things to note,

* `volumeHandle` is the volume name in storage array you want to reuse. Make sure it matches exactly the volume you're targeting. You can list the luns on NGX Storage management interface and get name of the lun you want to use.
* `fsType` important topic. If you don't want to format on attaching to container so use `noformat` otherwise use `ext4`, `ext3` or `xfs`. If `fsType` setted another supported filesystem driver will format on attach. Volume expand with `noformat` is possible and driver can resize your filesystem if exist.
* `storage` make sure it's set to the same storage size as your existing NGX Storage Block Storage volume.

Create a file with this content, naming it `pv.yaml` and deploying it:

```bash
kubectl create -f pv.yaml
```

View information about the `PersistentVolume`:

```bash
$ kubectl get pv volume-pv-01

NAME             CAPACITY   ACCESS MODES   RECLAIM POLICY   STATUS      CLAIM     STORAGECLASS                  REASON    AGE
volume-pv-01     5Gi        RWO            Delete           Available             iscsi-csi-ngxstorage-class              15s
```

The status is `Available`. This means it has not yet been bound to a PersistentVolumeClaim. Now we can proceed to create our PVC:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: csi-pod-pvc
spec:
  accessModes:
  - ReadWriteOnce
  resources:
    requests:
      storage: 5Gi
  storageClassName: iscsi-csi-ngxstorage-class
```

This is the same (just like our other examples). When you create `PVC`, Kubernetes will try to match it to an existing `PV`. Because the storageClassName and the storage size matches our `PV` descriptions, kubernetes will bind this `PVC` to our manually create `PV`. CSI **will not** create a new volume because of the existing `PV`. Make sure `storage` tag is set to the same storage size as your existing NGX Storage Block Storage volume and `PV` yaml file.

Create the PersistentVolumeClaim:

```bash
kubectl create -f pvc.yaml
```

Now look at the PersistentVolumeClaim (PVC):

```bash
kubectl get pvc task-pv-claim
NAME          STATUS    VOLUME           CAPACITY   ACCESS MODES   STORAGECLASS                 AGE
csi-pod-pvc   Bound     volume-pv-01     5Gi        RWO            iscsi-csi-ngxstorage-class   5s
```

As you see, the output shows that the PVC is bound to our PersistentVolume, `volume-pv-01`.

Finally, define your pod that refers to this PVC:

```yaml
kind: Pod
apiVersion: v1
metadata:
  name: my-csi-app
spec:
  containers:
  - name: my-frontend
    image: busybox
    volumeMounts:
    - mountPath: "/data"
      name: my-ngx-volume
    command: ["sleep", "1000000"]
  volumes:
  - name: my-ngx-volume
    persistentVolumeClaim:
      claimName: csi-pod-pvc
```

Check if the pod is running successfully:

```bash
kubectl describe pods/my-csi-app
```

Write inside the app container:

```sh
$ kubectl exec -ti my-csi-app /bin/sh
/ # ls /data
hello-world test
```
