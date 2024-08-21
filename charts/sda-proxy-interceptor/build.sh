#!/bin/bash

IMAGE="hub.cerit.io/sda/sda-proxy-interceptor:latest"
docker build -t "$IMAGE" -f interceptor/Dockerfile interceptor
docker push "$IMAGE"
