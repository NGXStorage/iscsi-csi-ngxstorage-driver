# NGX STORAGE BLOCK STORAGE CSI DRIVER

A Container Storage Interface (CSI) Driver for NGX Storage Block Storage. The CSI driver allows you to use NGX Storage Block Storage with your preferred Container Orchestrator.

The NGX Storage CSI driver is mostly tested on Kubernetes. Theoretically it should also work on other Container Orchestrators, such as Mesos or Cloud Foundry. Feel free to test it on other CO's and give us a feedback.

## Features

Below is a list of functionality implemented by the driver. In general, [CSI features](https://kubernetes-csi.github.io/docs/features.html) implementing an aspect of the specification.  

See also the [project examples](https://github.com/ngxstorage/iscsi-csi-ngxstorage-driver/tree/main/examples). for use cases.

### Volume Expansion

Volumes can be expanded by updating the storage request value of the corresponding PVC:

Before:

```yml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: csi-pvc
  namespace: default
spec:
  [...]
  resources:
    requests:
      # The field below can be increased.
      storage: 100Gi
      [...]
```

After:

```yml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: csi-pvc
  namespace: default
spec:
  [...]
  resources:
    requests:
      # The field below can be increased.
      storage: 200Gi
      [...]
```

After successful expansion, the status section of the PVC object will reflect the actual volume capacity.

Important notes:

- Volumes can only be increased in size, not decreased; attempts to do so will lead to an error.
- Expanding a volume that is larger than the target size will have no effect. The PVC object status section will continue to represent the actual volume capacity.
- Resizing volumes other than through the PVC object (e.g., the NGX Storage control panel) is not recommended as this can potentially cause conflicts. Additionally, size updates will not be reflected in the PVC object status section immediately, and the section will eventually show the actual volume capacity.

### Raw Block Volume

Volumes can be used in raw block device mode by setting the `volumeMode` on the corresponding PVC:

```yml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: csi-pvc
  namespace: default
spec:
  [...]
  volumeMode: Block
```

Important notes:

- If using volume expansion functionality, only expansion of the underlying persistent volume is guaranteed. We do not guarantee to automatically expand the filesystem if you have formatted the device.

### Existing Volume Transfer

Volumes can be transferred across clusters. The exact steps are outlined in our [example](https://github.com/ngxstorage/iscsi-csi-ngxstorage-driver/tree/master/examples/pod-single-existing-volume).

## Contributing

At NGX Storage we value and love our community! If you have any issues or would like to contribute, feel free to open an issue or send an e-mail [k8s@ngxstorage.com](mailto:k8s@ngxstorage.com).

## Installing to Kubernetes

### Kubernetes Compatibility

The following table describes the required NGX Storage CSI driver version per supported Kubernetes release.

| Kubernetes Release | NGX Storage CSI Driver Version |
|--------------------|--------------------------------|
| 1.26               | v1.0.0+                        |
| 1.27               | v1.0.0+                        |
| 1.28               | v1.0.0+                        |

### Requirments

NGX Storage Block Storage Driver tested on Ubuntu, Pardus and RHEL 9 hosts.

#### Storage

- Create a pool.
- Create a portal group. Check network k8s hosts can reach storage.

#### Controller Mode

- No spesific need

#### Node Mode

- open-iscsi package or equvilient
- multipath-tools package or equvilient
- add this lines

  ```conf
  defaults {
    user_friendly_names yes
    find_multipaths yes
  }
  ```

  to `/etc/multipath.conf` file for find multipath devices in proper way.
- be sure `/etc/iscsi/initiatorname.iscsi` file has `InitiatorName=iqn.1994-05.com.redhat:HOSTNAME` line.

### 1. Clone this repo for deploy driver

```bash
git clone https://github.com/NGXStorage/iscsi-csi-ngxstorage-driver
```

### 2. Secret file deploy

#### 2.1. Edit `deploy/iscsi-csi-ngxstorage-secret.yaml`

Edit secret file with your storage informations.

#### 2.2. Create Secret

```bash
$ kubectl create -f deploy/{iscsi-csi-ngxstorage-secret.yaml}
secret/ngxstorage-secret created
```

You should now see the secret in the `kube-system` namespace along with other secrets

```bash
$ kubectl -n kube-system get secrets
NAME                  TYPE                                  DATA      AGE
default-token-jskxx   kubernetes.io/service-account-token   3         18h
ngxstorage-secret     Opaque                                1         18h
```

### 3. Deploy the CSI driver and sidecars

#### 3.1 Edit StorageClass

Edit file acording to needs its equvilent of NGX Storage manager LUN create page.

#### 3.2. Deploy Driver

```bash
kubectl apply -f deploy/

storageclass.storage.k8s.io/iscsi-csi-ngxstorage-class created
deployment.apps/iscsi-csi-ngxstorage-controller created
daemonset.apps/iscsi-csi-ngxstorage-node created
csidriver.storage.k8s.io/iscsi.csi.ngxstorage.com created
serviceaccount/iscsi-csi-ngxstorage-service-account created
clusterrole.rbac.authorization.k8s.io/iscsi-csi-ngxstorage-cluster-role created
clusterrolebinding.rbac.authorization.k8s.io/iscsi-csi-ngxstorage-cluster-role-binding created
```

### 4. Test and Verify

#### 4.1. Create a PersistentVolumeClaim

This makes sure a volume is created and provisioned on your behalf:

##### 4.1.1 Edit `examples/pod-single-volume/pvc.yaml`

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: test-filesystem-pvc
spec:
  storageClassName: iscsi-csi-ngxstorage-class
  volumeMode: Filesystem
  accessModes:
  - ReadWriteOnce
  resources:
    requests:
      storage: 10Gi
```

##### 4.1.2 Deploy PVC

```bash
kubectl apply -f examples/pod-single-volume/pvc.yaml
```

##### 4.1.3 Check PVC

Check that a new PersistentVolume is created based on your claim:

```bash
$ kubectl get pv
NAME                                       CAPACITY   ACCESS MODES   RECLAIM POLICY   STATUS    CLAIM             STORAGECLASS                 REASON    AGE
pvc-07b52079-1198-72e8-b6b4-5d1af75f32d8   5Gi        RWO            Delete           Bound     default/csi-pvc   iscsi-csi-ngxstorage-class             3m
```

#### 4.2 Create a POD

The above output means that the CSI driver successfully created (provisioned) a new Volume on behalf of you. You should be able to see this newly created volume under the SAN tab LUNS section in the NGX Storage Management UI.

##### 4.2.1 Edit `examples/pod-single-volume/pod.yaml`

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
        name: my-do-volume
      command: [ "sleep", "1000000" ]
  volumes:
    - name: my-do-volume
      persistentVolumeClaim:
        claimName: test-filesystem-pvc
```

##### 4.2.2 Deploy POD

```bash
kubectl apply -f examples/pod-single-volume/pod.yaml
```

##### 4.2.3 Check POD

Check if the pod is running successfully:

```bash
kubectl describe pods/my-csi-app
```

#### 4.3 Try to write data to the volume in container

```bash
$ kubectl exec -ti my-csi-app /bin/sh
/ # touch /data/hello-world
/ # dd if=/dev/zero of=/data/test bs=1M count=1000
/ # exit
$ kubectl exec -ti my-csi-app /bin/sh
/ # ls /data
hello-world test
```

Now check the storage UI to see if the data is written to the volume.

## Licensing

Copyright 2023 NGX Teknoloji A.Ş.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

<http://www.apache.org/licenses/LICENSE-2.0>

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
