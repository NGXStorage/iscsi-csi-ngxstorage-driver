#!/usr/bin/env bash
set -euo pipefail

oc apply -f benchmark-kbench-namespace.yaml
oc apply -f benchmark-kbench-serviceaccount.yaml
oc -n benchmark-kbench-iscsi delete pod benchmark-kbench-iscsi --ignore-not-found --wait=false
oc apply -f benchmark-kbench-sc.yaml -f benchmark-kbench-pvc.yaml -f benchmark-kbench-pod.yaml
oc wait --for=jsonpath='{.status.phase}'=Bound pvc/benchmark-kbench-iscsi-pvc -n benchmark-kbench-iscsi --timeout=10m
oc wait --for=jsonpath='{.status.phase}'=Succeeded pod/benchmark-kbench-iscsi -n benchmark-kbench-iscsi --timeout="${KBENCH_TIMEOUT:-1200}s"
oc logs pod/benchmark-kbench-iscsi -n benchmark-kbench-iscsi
oc describe pod/benchmark-kbench-iscsi -n benchmark-kbench-iscsi | tail -40
