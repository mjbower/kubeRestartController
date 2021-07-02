#Cluster Restart Controller

## Description
Controller to monitor pod constant restarts,  scale them to zero and alert.


Notes:
must be labelled as "app=xx" otherwise it can't be targetted


which deployments have been scaled to zero ??

kubectl get deploy --show-labels

```bigquery
NAME         READY   UP-TO-DATE   AVAILABLE   AGE     LABELS
badpod       0/0     0            0           8m37s   ClusterScaledToZero=true,app=badpod
```

```

## Use cases
1. Pod fails over and over
2. Prod has intermittent failure over a long period of time
3. label a namespace to allow all pods to be ignored ?
4. set a timer,  if a pod restarts repeatedly in 1 hour,  then remove it ?


## How to deploy
xx

## Features

##Ignore certain deployments
kubectl label pod xxx ignoreme=true
kubectl label pod xxx ignoreme=false --overwrite=true


## Create a failing pod
kubectl run bob --image=alpine --command "xxxxx"
