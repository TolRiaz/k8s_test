#!/bin/bash

kubectl delete -f iperf-server.yaml
kubectl delete -f iperf-client.yaml

kubectl create -f iperf-server.yaml
kubectl create -f iperf-client.yaml
