#!/bin/bash
kubectl delete -f prometheus-deployment.yaml
kubectl delete -f prometheus-server-svc.yaml

kubectl create -f prometheus-deployment.yaml
kubectl create -f prometheus-server-svc.yaml
