

Hygon HCU DRA Driver 是一个 Kubernetes Dynamic Resource Allocation (DRA) Kubelet 插件，以 DaemonSet 方式部署到 HCU 节点，将物理 HCU 注册为 ResourceSlice，并通过 CDI（Container Device Interface） 将设备注入工作负载容器。该方案面向 Hygon HCU 在 Kubernetes 上提供基于DRA （Dynamic Resource Allocation） 与 CDI（Container Device Interface） 的统一设备分配能力。在创建时自动创建并注入 ResourceClaim，由调度器完成设备绑定，节点插件通过 DCGM 发现物理卡、按需创建 vHCU 或整卡，生成 CDI 规范并返回 CDIDeviceIDs，由容器运行时注入 /dev/kfd、/dev/dri 等设备与挂载。该方案将传统 device-plugin 扩展资源路径收敛到 DRA + CDI 体系，并与 hcu-exporter 等组件通过 /etc/vdev/dynamic 等机制保持兼容；部署时需k8s集群版本高于v1.34.0，确认开启 DRA 相关 Feature Gate，节点运行 driver DaemonSet，且容器运行时启用 CDI。

## 4.1 概述

社区已经提供针对Kubernetes DRA（Dynamic Resource Allocation，动态资源分配）功能的支持从而实现 HCU 作负载的动态资源分配。它可以让你在 DRA 模式下获得相比于传统 DevicePlugin 使用方式更便捷的使用体验。

•**物理 HCU 发现与注册****：**通过 DCGM 枚举节点上的空闲物理卡，发布到 ResourceSlice（驱动名 dra.hygon.com）；

•**整卡直通****：**ResourceClaim 不指定 capacity 时，整卡分配，不创建 vHCU；

•**按容量动态切分vHCU****：**在 Claim 中请求 cores / memory 时，Prepare 阶段动态创建 vHCU，Unprepare 后回收；

•**CDI 设备注入****：**Prepare 返回 CDIDeviceIDs，由容器运行时根据 CDI spec 注入 /dev/kfd、/dev/dri/*、hyhal 等；

•**与 hcu-exporter 联动****：**在 /etc/vdev/dynamic/ 维护 HAMI 风格标记文件，供 hcu-exporter 扫描;；

•**状态持久化与清理****：**驱动重启后恢复已 Prepare 的 Claim；定期清理无对应 Pod 的残留 vHCU；

## 4.2 安装部署

本文档涉及的yaml文件可以从社区资源中心进行下载，镜像可以从光源镜像仓库直接拉取。

### 4.2.1 前提条件

•Kubernetes 版本 >= 1.34 并且 DRA Consumable Capacity 和 Dynamic Resource Allocation featuregate 已启用

•底层容器运行时（例如 containerd 或 CRI-O）必须启用 CDI

### 4.2.2 安装

安装 HCU-DRA-Driver

```
# 此项为固定标签,部署设备插件时需要查询此标签 
kubectl label nodes <node-name> hcu=on

# 安装 hcu-dra-driver
kubectl apply -f hcu-dra-driver.yaml

# 验证 hcu-dra-driver 安装状态
root@node1:~# kubectl get po -A |grep hcu-dra-driver
kube-system    hcu-dra-driver-hbfk9                       1/1     Running   0               17h

#验证 DeviceClass是否成功创建
root@node1:~# kubectl get deviceclass
NAME               AGE
hcu-dra.hygon.io   18h

# 检查 HCU 设备是否已发布为 ResourceSlice：
root@node1:~# kubectl get resourceslices
NAME                           NODE    DRIVER             POOL    AGE
node1-hcu-dra.hygon.io-9h4jz   node1   hcu-dra.hygon.io   node1   17h
```

## 4.3 使用方式

HCU-DRA-Driver 支持两种使用模式: DRA原生模式 和 Device Plugin 兼容模式。

### 4.3.1 DRA 原生模式

创建 ResourceClaim 来请求指定 cores 和 memory 的 GPU：

```
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: hcu-claim-basic
  namespace: hcu-dra-test
spec:
  devices:
    requests:
      - name: hcu
        exactly:
          deviceClassName: hcu-dra.hygon.io
          allocationMode: ExactCount
          count: 1
          selectors:
            - cel:
                expression: |
                  device.driver == "hcu-dra.hygon.io" &&
                  device.attributes["hcu-dra.hygon.io"].type == "hcu"
          capacity:
            requests:
              cores: "60"
              memory: "20Gi"
```

在Pod中引用该 ResourceCliam ：

```
apiVersion: v1
kind: Pod
metadata:
  name: hcu-consumer
  namespace: hcu-dra-test
spec:
  restartPolicy: Never
  resourceClaims:
    - name: hcu-claim-ref
      resourceClaimName: hcu-claim-basic  # 引用 ResourceClaim 
  containers:
    - name: app
      image: harbor.sourcefind.cn:5443/hcu/admin/base/pytorch:2.1.0-ubuntu22.04-dtk24.04.2-py3.10
      command: [ "/bin/bash", "-c", "--" ]
      args: [ "sleep infinity & wait" ]
      resources:
        claims:
          - name: hcu-claim-ref
```

### 4.3.2 Device Plugin 兼容模式

该模式需要结合[HAMi-DRA](https://github.com/Project-HAMi/HAMi-DRA) 进行使用。HAMi-DRA 会自动将  Device Plugin 风格的资源请求转换为 DRA ResourceClaim ，可以使用与传统模式完全相同的资源语法。

安装 cert-manager

HAMi-DRA 依赖 cert-manager 提供 TLS 证书，需要提前安装

```
# 安装 cert-manager
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.14.6/cert-manager.yaml

# 验证 cert-manager 安装状态
root@node1:~# kubectl get po -n cert-manager
NAME                                       READY   STATUS    RESTARTS   AGE
cert-manager-bdcffbc7f-fq8v5               1/1     Running   0          47h
cert-manager-cainjector-7d7978c4c4-jnfrb   1/1     Running   0          47h
cert-manager-webhook-7d5646cb9b-bjqpp      1/1     Running   0          47h
```

安装HCU-DRA-Webhook

```
# 安装 hcu-dra-webhook 及相关配置
helm repo add hami-dra https://project-hami.github.io/HAMi-DRA
helm repo update
helm install hami-dra hami-dra/hami-dra

# 验证安装
$ kubectl get pods -n hami-system
NAME                                     READY   STATUS    RESTARTS   AGE
hami-dra-driver-kubelet-plugin-bzkr4     1/1     Running   0          73m
hami-hami-dra-monitor-7b484d5f95-bxx6z   1/1     Running   0          74m
hami-hami-dra-webhook-64bfdc6b86-fnwtp   1/1     Running   0          74m
```

使用示例：

```
apiVersion: v1
kind: Pod
metadata:
  name: hcu-consumer
spec:
  restartPolicy: Never
  containers:
    - name: app
      image: harbor.sourcefind.cn:5443/hcu/admin/base/pytorch:2.1.0-ubuntu22.04-dtk24.04.2-py3.10
      command: [ "/bin/bash", "-c", "--" ]
      args: [ "sleep infinity & wait" ]
      resources:
        limits:
          hygon.com/hcunum: "1"
          hygon.com/hcucores: "50"
          hygon.com/hcumem: "10240"
```

# 