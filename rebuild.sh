#!/bin/sh -x

export CGO_ENABLED=0
go build

APP=teleglogger
BAY=t0mk

now=$(date +'%Y-%m-%d_%T')
echo "Will rebuild ${BAY}/${APP}"
go build  -ldflags "-X main.buildTime=$now" && docker build -t ${BAY}/${APP} . 
#go build  -ldflags "-X main.buildTime=$now" && docker build -t ${BAY}/${APP} .  && docker push ${BAY}/${APP}
