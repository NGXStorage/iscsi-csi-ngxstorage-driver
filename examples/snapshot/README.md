# Snapshot usage

Below you will find the intruction on how to use snapshot feature with your Kubernetes cluster.

## Preconditions

Make sure the snapshot custom config yaml installed on your cluster. You can find it in [CSI Snapshotter](https://github.com/kubernetes-csi/external-snapshotter/tree/master/client/config/crd).

```bash
$ kubectl apply -f $(echo https://raw.githubusercontent.com/kubernetes-csi/external-snapshotter/master/client/config/crd/snapshot.storage.k8s.io_volumesnapshot{s,classes,contents}.yaml | tr ' ' ',')

customresourcedefinition.apiextensions.k8s.io/volumesnapshots.snapshot.storage.k8s.io created
customresourcedefinition.apiextensions.k8s.io/volumesnapshotclasses.snapshot.storage.k8s.io created
customresourcedefinition.apiextensions.k8s.io/volumesnapshotcontents.snapshot.storage.k8s.io created

$ kubectl apply -f $(echo https://raw.githubusercontent.com/kubernetes-csi/external-snapshotter/master/client/config/crd/groupsnapshot.storage.k8s.io_volumegroupsnapshot{s,classes,contents}.yaml | tr ' ' ',')

customresourcedefinition.apiextensions.k8s.io/volumegroupsnapshots.groupsnapshot.storage.k8s.io created
customresourcedefinition.apiextensions.k8s.io/volumegroupsnapshotclasses.groupsnapshot.storage.k8s.io created
customresourcedefinition.apiextensions.k8s.io/volumegroupsnapshotcontents.groupsnapshot.storage.k8s.io created

```

## Example

To use snapshot feature, need to create a `VolumeSnapshotClass` resource but its already created in deploy section. Now lets check it.

```bash
$ kubectl get volumesnapshotclass

NAME                                  DRIVER                     DELETIONPOLICY   AGE
iscsi-csi-ngxstorage-snapshot-class   iscsi.csi.ngxstorage.com   Retain           1s
```

As you see `iscsi-csi-ngxstorage-snapshot-class` is created already.

### Create a VolumeSnapshot

Now we can create a `VolumeSnapshot` resource like in [create-snapshot.yaml](https://github.com/NGXStorage/iscsi-csi-ngxstorage-driver/tree/main/examples/snapshot/create-snapshot.yaml) file (In this example we will use [deployment-single-volume](https://github.com/NGXStorage/iscsi-csi-ngxstorage-driver/tree/main/examples/deployment-single-volume) example).

First we need to enter name of the `VolumeSnapshotClass` to `volumeSnapshotClassName` field. Then we need to enter name of the `PersistentVolumeClaim` to `source.persistentVolumeClaimName` field. And lastly we need to enter name of the `VolumeSnapshot` to `metadata.name` field.

File should look like this:

```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: deployment-single-volume-snapshot
spec:
  volumeSnapshotClassName: iscsi-csi-ngxstorage-snapshot-class
  source:
    persistentVolumeClaimName: csi-deployment-pvc
```

Then we can create `VolumeSnapshot` resource.

```bash
$ kubectl apply -f create-snapshot.yaml

volumesnapshot.snapshot.storage.k8s.io/deployment-single-volume-snapshot created
```

Now we can check `VolumeSnapshot` resource.

```bash
$ kubectl get volumesnapshot
NAME                                READYTOUSE   SOURCEPVC            SOURCESNAPSHOTCONTENT   RESTORESIZE   SNAPSHOTCLASS                         SNAPSHOTCONTENT                                    CREATIONTIME   AGE
deployment-single-volume-snapshot   true         csi-deployment-pvc                           56Ki          iscsi-csi-ngxstorage-snapshot-class   snapcontent-d33c3da5-49f9-48bd-9779-87a3e2bff99e   84s            87s
```

As you see `deployment-single-volume-snapshot` is created.

### Restore PVC from VolumeSnapshot

Now we can restore a `PersistentVolumeClaim` from `VolumeSnapshot` resource. In this example we will use [deployment-single-volume](https://github.com/NGXStorage/iscsi-csi-ngxstorage-driver/tree/main/examples/deployment-single-volume) as PVC to restore.

First we need to create a `PersistentVolumeClaim` resource like in [restore-snapshot.yaml](https://github.com/NGXStorage/iscsi-csi-ngxstorage-driver/tree/main/examples/snapshot/restore-snapshot.yaml) file.

File should look like this:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: restore-deployment-single-volume-snapshot
spec:
  storageClassName: iscsi-csi-ngxstorage-class
  dataSource:
    name: deployment-single-volume-snapshot
    kind: VolumeSnapshot
    apiGroup: snapshot.storage.k8s.io
  accessModes:
  - ReadWriteOnce
  resources:
    requests:
      storage: 5Gi
```

Then we can create `PersistentVolumeClaim` resource.

```bash
$ kubectl apply -f restore-snapshot.yaml

persistentvolumeclaim/restore-deployment-single-volume-snapshot created
```

Now we can check `PersistentVolumeClaim` resource.

```bash
$ kubectl get pvc
NAME                                        STATUS   VOLUME                                     CAPACITY   ACCESS MODES   STORAGECLASS                 AGE
restore-deployment-single-volume-snapshot   Bound    pvc-86b10904-4e2c-4bac-9bf1-a1dc61edc16e   1Gi        RWO            iscsi-csi-ngxstorage-class   17s
csi-deployment-pvc                          Bound    pvc-d25fd45a-495a-48a5-bd34-31bea6e4b171   1Gi        RWO            iscsi-csi-ngxstorage-class   13m
```

As you see `restore-deployment-single-volume-snapshot` is created and bound to a `PersistentVolume`.
