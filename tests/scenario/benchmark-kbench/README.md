# benchmark-kbench (longhornio/benchmark-kbench) — iSCSI CSI driver

`longhornio/benchmark-kbench` ile NGX Storage iSCSI CSI driver'ın performans doğrulaması:
randread/randwrite IOPS, seq read/write bandwidth, read/write latency.

## Çalıştırma

```bash
# iSCSI yalnızca: kendi namespace/SC/PVC/job isimleri
oc apply -f benchmark-kbench-namespace.yaml -f benchmark-kbench-serviceaccount.yaml -f benchmark-kbench-sc.yaml -f benchmark-kbench-pvc.yaml -f benchmark-kbench-pod.yaml

# job bitene kadar bekle (MODE=full ~12-35 dk)
oc wait --for=jsonpath='{.status.phase}'=Succeeded pod/benchmark-kbench-iscsi -n benchmark-kbench-iscsi --timeout=1200s

# sonucu al
oc logs pod/benchmark-kbench-iscsi -n benchmark-kbench-iscsi --tail=40

bash run-benchmark-kbench.sh
```

The job has a dedicated service account. Do not grant `anyuid` to the default
service account; the driver image must provide the requested mount permissions
before this benchmark is run under restricted SCC.

## Beklenen çıktı (örnek iSCSI 40G)

```
IOPS (Read/Write)
        Random:         117,464 / 21,523
Bandwidth in KiB/sec (Read/Write)
    Sequential:       33,800 / 1,492,617
Latency in ns (Read/Write)
        Random:         75,234 / 244,264
```

## Notlar
- Sonuçlar `tests/scenario/benchmark-kbench/logs/` altında saklanır.
- Seq read değeri LUN'un okuma cache durumuna bağlıdır (write flash-cache'li, read HDD);
  derin queue (iodepth=16) ile 2.8 GB/s okuma görülür, benchmark-kbench iodepth=1 kullanır.
- NFS ile karşılaştırma: NFS 40G — IOPS 104k/535, Seq BW 2,207,924/44,470.
