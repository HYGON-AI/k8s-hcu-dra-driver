# Hygon HCU DRA Driver

Hygon HCU DRA Driver 是一个 Kubernetes **Dynamic Resource Allocation (DRA)** Kubelet 插件，以 DaemonSet 方式部署到 HCU 节点，将物理 HCU 注册为 `ResourceSlice`，并通过 **CDI（Container Device Interface）** 将设备注入工作负载容器。

当前版本：**v1.0.0**

默认镜像：`harbor.sourcefind.cn:5443/hcu/admin/base/hcu-dra-driver:v1.0.0`

## 功能特性

| 能力 | 说明 |
|------|------|
| 物理 HCU 发现与注册 | 通过 DCGM 枚举节点上的空闲物理卡，发布到 `ResourceSlice`（驱动名 `dra.hygon.com`） |
| 整卡直通 | `ResourceClaim` 不指定 `capacity` 时，整卡分配，不创建 vHCU |
| 按容量动态切分 vHCU | 在 Claim 中请求 `cores` / `memory` 时，Prepare 阶段动态创建 vHCU，Unprepare 后回收 |
| CDI 设备注入 | Prepare 返回 `CDIDeviceIDs`，由容器运行时根据 CDI spec 注入 `/dev/kfd`、`/dev/dri/*`、hyhal 等 |
| 与 hcu-exporter 联动 | 在 `/etc/vdev/dynamic/` 维护 HAMI 风格标记文件，供 [hcu-exporter](../hcu-exporter) 扫描 |
| 状态持久化与孤儿清理 | 驱动重启后恢复已 Prepare 的 Claim；定期清理无对应 Pod 的残留 vHCU |

## 与 Device Plugin 的对比

| 项目 | [k8s-hcu-device-plugin](../k8s-hcu-device-plugin) | 本项目（DRA Driver） |
|------|---------------------------------------------------|----------------------|
| Kubernetes API | Device Plugin API（`hygon.cn/hcu` 等资源） | DRA API（`DeviceClass` / `ResourceClaim` / `ResourceSlice`） |
| 设备注入方式 | Allocate 返回 `Mounts` / `DeviceSpec` | Prepare 返回 `CDIDeviceIDs`，依赖运行时 CDI |
| 动态 vHCU 调度 | 需配合 [k8s-hcu-scheduler](../k8s-hcu-scheduler) + 注解 | 由 DRA 调度器按 `ResourceSlice` 容量分配，无需自定义 Scheduler |
| 最低 K8s 版本 | 较宽 | 建议 **1.34+**（DRA GA 及 `DRAConsumableCapacity`） |

两者可并存于不同工作负载：新应用推荐 DRA；存量 Device Plugin 工作负载可逐步迁移。

## 架构概览

```
┌──────────────────────────────────────────────────────────────────────┐
│                         Kubernetes Node (hcu=on)                    │
│  ┌────────────┐   ResourceSlice    ┌─────────────────────────────┐ │
│  │  Scheduler │◄──────────────────►│  hcu-dra-driver (DaemonSet)  │ │
│  └─────┬──────┘                    │  - 枚举 HCU、发布 ResourceSlice│ │
│        │ ResourceClaim 分配        │  - Prepare/Unprepare Claims  │ │
│        ▼                           │  - 写 CDI spec → /var/run/cdi│ │
│  ┌────────────┐   CDIDeviceIDs     │  - 动态创建/销毁 vHCU         │ │
│  │  Kubelet   │◄──────────────────►└──────────────┬──────────────┘ │
│  └─────┬──────┘                                    │ DCGM           │
│        │ CDI 注入                                   ▼                │
│  ┌─────▼────────────────────────────────────────────────────────┐  │
│  │  Workload Pod  (/dev/kfd, /dev/dri/*, /opt/hyhal, /etc/vdev)  │  │
│  └────────────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────────────┘
```

**分配模式：**

| 模式 | Claim 写法 | Prepare 行为 | 适用场景 |
|------|-----------|-------------|---------|
| 整卡直通 | 无 `exactly.capacity` | 引用物理卡 CDI spec，不创建 vHCU | 独占整卡训练/推理 |
| 按容量切分 | 含 `capacity.requests.cores` / `memory` | 动态创建 vHCU，写入 per-vdevice CDI spec | 多租户共享单卡 |

## 前置要求

| 项目 | 说明 |
|------|------|
| Kubernetes | **1.34+**，已启用 DRA 相关 Feature Gate（见下表） |
| DRA CRD | `DeviceClass`、`ResourceClaim`、`ResourceSlice` 等已就绪 |
| HCU 驱动 | 节点已安装 HCU 驱动，`/sys/class/kfd` 存在 |
| hyhal | 节点存在 `/opt/hyhal` 或 `/usr/local/hyhal` |
| 容器运行时 CDI | **生产环境必须启用**（见 [CDI 配置](#cdi-配置)） |
| 节点标签 | DaemonSet 使用 `nodeSelector: hcu: "on"`，部署前执行 `kubectl label node <name> hcu=on` |
| 空闲物理卡 | 节点上 **不能已有预切分 vHCU**（`VDeviceCount != 0` 的卡会被跳过）；驱动启动时会清理无 Claim 跟踪的残留 vHCU |

### Feature Gate

| 组件 | 必需门控 | 说明 |
|------|---------|------|
| kube-apiserver | `DynamicResourceAllocation` | DRA API 基础能力 |
| kube-scheduler | `DynamicResourceAllocation` | Claim 调度分配 |
| kube-apiserver、kube-scheduler、**kubelet** | `DRAConsumableCapacity` | 按 `cores` / `memory` 切分 vHCU 时 **三门控必须全部启用** |

未启用 `DRAConsumableCapacity` 时，apiserver 会丢弃 `ResourceSlice` 中的 `allowMultipleAllocations` 与 `requestPolicy`，插件日志可能出现 `some fields were dropped ... DRAConsumableCapacity`。此时应去掉 Claim 中的 `exactly.capacity` 块，或全链路打开门控。

## 快速开始

### 1. 构建镜像

在 **Linux 宿主机**（需 CGO 编译环境与 hcu-dcgm 头文件/库）执行：

```bash
cd k8s-hcu-dra-driver
bash build.sh          # 默认：编译 + 镜像 + 导出 tar
bash build.sh binary   # 仅编译二进制
bash build.sh image    # 仅构建镜像（需已有 bin/hcu-dra-driver）
bash build.sh tar      # 仅导出镜像 tar（需已有本地镜像）
```

产物：

| 阶段 | 路径 | 说明 |
|------|------|------|
| 二进制 | `bin/hcu-dra-driver` | 宿主机 `go build` 输出 |
| 镜像 tar | `dist/hcu-dra-driver-v1.0.0.tar` | `docker save` 导出 |

自定义版本标签：

```bash
IMAGE_TAG=v1.0.0 bash build.sh
```

> **提示**：若脚本报 `invalid option name: pipefail`，多为 Windows CRLF 换行导致，执行 `sed -i 's/\r$//' build.sh scripts/build.sh` 或 `dos2unix build.sh` 后重试。

构建流程与 [k8s-hcu-device-plugin](../k8s-hcu-device-plugin) 一致：**宿主机编译** → **Dockerfile 仅打包二进制**，不在容器内编译。可选将 hcu-dcgm 运行时库放入 `lib/` 并在 Dockerfile 中 `COPY lib/ /usr/local/lib/`（当前默认依赖节点 `/opt/hyhal/lib`）。

### 2. 部署驱动

```bash
kubectl apply -f deployments/static/hcu-dra-driver.yaml
```

该清单创建：

| 资源 | 名称 | 说明 |
|------|------|------|
| `DeviceClass` | `dra.hygon.com` | HCU 设备类 |
| `DaemonSet` | `hcu-dra-driver`（`kube-system`） | 节点插件，`nodeSelector: hcu=on` |
| RBAC | ServiceAccount / ClusterRole / ClusterRoleBinding | ResourceSlice 读写、Claim 状态更新 |

**kubelet 根目录对齐**：若 kubelet 使用非默认 `--root-dir`，须同步修改 DaemonSet 中 `plugins_registry`、`plugins` 的 hostPath 及对应环境变量，否则会出现 `DRA driver dra.hygon.com is not registered`。

### 3. 验证部署

```bash
# DaemonSet 就绪
kubectl -n kube-system get ds hcu-dra-driver

# DeviceClass 存在
kubectl get deviceclass dra.hygon.com

# ResourceSlice 已发布且含设备
kubectl get resourceslice -o yaml

# 驱动日志无报错
kubectl -n kube-system logs ds/hcu-dra-driver --tail=200
```

期望：`ResourceSlice` 的 `spec.devices` 非空，日志无 `0 allocatable device(s)`（后者常见于节点上已有未清理的 vHCU）。

## 使用示例

仓库提供以下示例清单（位于 `deployments/examples/`）：

| 文件 | 用途 |
|------|------|
| `resourceclaim-hcu-basic.yaml` | 按容量切分 vHCU（含 `capacity.requests`） |
| `resourceclaim-hcu-physical.yaml` | 整卡直通（无 `capacity`） |
| `pod-hcu-consumer.yaml` | CDI 路径消费 Claim（推荐） |
| `pod-hcu-physical-consumer.yaml` | 整卡 CDI 冒烟测试 |
| `pod-hcu-consumer-manual-hostmounts.yaml` | 未开 CDI 时的权宜 hostPath 方案 |
| `pod-hcu-physical-consumer-manual-hostmounts.yaml` | 整卡 + 手工 hostPath |

### 按容量切分 vHCU

```bash
kubectl create ns hcu-dra-test --dry-run=client -o yaml | kubectl apply -f -

# 按节点 ResourceSlice 中的 capacity 调整 cores/memory
kubectl apply -f deployments/examples/resourceclaim-hcu-basic.yaml
kubectl apply -f deployments/examples/pod-hcu-consumer.yaml

kubectl -n hcu-dra-test get resourceclaim hcu-claim-basic -o yaml
kubectl -n hcu-dra-test get pod hcu-consumer -w
```

`resourceclaim-hcu-basic.yaml` 默认请求 `cores: "60"`、`memory: "20Gi"`，请根据实际设备容量修改。

### 整卡直通

```bash
kubectl apply -f deployments/examples/resourceclaim-hcu-physical.yaml
kubectl apply -f deployments/examples/pod-hcu-physical-consumer.yaml
```

整卡模式下 Prepare 日志为 `Whole-card pass-through`，节点上不会新增 vdevice。

### ResourceClaim 关键字段

**按容量切分（需 `DRAConsumableCapacity`）：**

```yaml
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
          deviceClassName: dra.hygon.com
          allocationMode: ExactCount
          count: 1
          selectors:
            - cel:
                expression: |
                  device.driver == "dra.hygon.com" &&
                  device.attributes["dra.hygon.com"].type == "hcu"
          capacity:
            requests:
              cores: "60"
              memory: "20Gi"
```

**整卡直通（省略 `capacity` 块）：**

```yaml
# 与 resourceclaim-hcu-physical.yaml 相同结构，不含 capacity
```

### 消费 Claim 的 Pod

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: hcu-consumer
  namespace: hcu-dra-test
spec:
  restartPolicy: Never
  resourceClaims:
    - name: hcu-claim-ref
      resourceClaimName: hcu-claim-basic
  containers:
    - name: app
      image: ubuntu:22.04
      command: ["sleep", "infinity"]
      resources:
        claims:
          - name: hcu-claim-ref
```

> **注意**：仅创建 `ResourceClaim`、尚无 Pod 引用时，Claim 可能长期 `pending` 且无 `.status.allocation`。须先创建引用该 Claim 的 Pod，调度器才会完成分配。

## CDI 配置

本驱动 **仅通过 CDI 注入设备**，Prepare 不返回 `Mounts` / `DeviceSpec`。生产环境须在容器运行时启用 CDI。

驱动启动时向 `CDI_ROOT`（默认 `/var/run/cdi`）写入：

- `hcu-dra-driver-device.yaml` — 物理卡 CDI spec
- `hcu-dra-driver-vdevice-*.yaml` — 各 vHCU 的 CDI spec

### containerd 示例

在 **工作节点** 检查 `/etc/containerd/config.toml`：

```bash
grep -nEi 'enable_cdi|cdi_spec' /etc/containerd/config.toml
```

期望：

```toml
enable_cdi = true
cdi_spec_dirs = ["/etc/cdi", "/var/run/cdi"]
```

修改后 `systemctl restart containerd`。`cdi_spec_dirs` 须包含 DaemonSet 挂载的 `/var/run/cdi`。

### 未启用 CDI 时

kubelet 可能完成 DRA 分配，但容器内看不到 `/dev/kfd` 等设备。可使用 `pod-hcu-consumer-manual-hostmounts.yaml` 做冒烟排障（`privileged` + 固定 hostPath），**不适合生产**：多卡时 `/dev/dri` 绑定不如 CDI 精细。

### 容器内验证

```bash
kubectl -n hcu-dra-test exec -it hcu-consumer -- bash
ls -l /dev/kfd /dev/dri
# 若镜像带 hy-smi：
# source /opt/hyhal/env.sh && hy-smi
```

vHCU 模式下，在承载 Pod 的节点执行 `hy-smi virtual -show-vdevice-info` 可看到动态创建的 vdevice；Pod 删除后应被回收。

## 配置参考

DaemonSet 环境变量（`deployments/static/hcu-dra-driver.yaml`）：

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `NODE_NAME` | 来自 Downward API | 当前节点名 |
| `NAMESPACE` | 来自 Downward API | Pod 所在命名空间 |
| `CDI_ROOT` | `/var/run/cdi` | CDI spec 写入目录 |
| `KUBELET_REGISTRAR_DIRECTORY_PATH` | `/var/lib/kubelet/plugins_registry` | 插件注册目录 |
| `KUBELET_PLUGINS_DIRECTORY_PATH` | `/var/lib/kubelet/plugins` | 插件数据目录 |
| `LOG_LEVEL` | `5` | klog 详细级别（1–5，调试 Prepare/Unprepare 建议 ≥ 4） |

## 验证 DRAConsumableCapacity

按容量切分前，确认三门控已生效且 `ResourceSlice` 字段未被丢弃。

### 检查 Feature Gate

```bash
# control plane
grep -n 'DRAConsumableCapacity' /etc/kubernetes/manifests/kube-apiserver.yaml
grep -n 'DRAConsumableCapacity' /etc/kubernetes/manifests/kube-scheduler.yaml

# worker 节点
grep -A5 'featureGates:' /var/lib/kubelet/config.yaml
```

### 检查 ResourceSlice

```bash
kubectl get resourceslice -o yaml
```

`spec.devices[]` 中应包含：

- `allowMultipleAllocations: true`
- `capacity.cores` / `capacity.memory` 含 `requestPolicy`（`default`、`validRange` 等）

若仅有 `value` 无 `requestPolicy`，说明门控未生效或驱动镜像过旧。

### 检查 Claim 与分配结果

```bash
kubectl -n hcu-dra-test get resourceclaim hcu-claim-basic \
  -o jsonpath='{.spec.devices.requests[0].exactly.capacity}' ; echo

kubectl -n hcu-dra-test get resourceclaim hcu-claim-basic -o yaml
```

期望：`capacity` 字段非空；`.status.allocation.devices` 中出现已分配设备，且含 `consumedCapacity`（字段名因 K8s 小版本可能略有差异）。

### 同卡多次分配（可选）

在单卡容量足够时，创建第二个 Claim，`capacity.requests` 更小，两份之和不超过 `ResourceSlice` 公布的总量。两个 Claim 均 `Allocated` 即说明 consumable 路径可用。

## 故障排查

### ResourceClaim 一直 pending

1. 是否已创建引用该 Claim 的 Pod？
2. `kubectl describe resourceclaim -n <ns> <name>` 查看事件
3. `kubectl get resourceslice` 确认有可分配设备
4. `capacity.requests` 是否超过设备总量（对照 `ResourceSlice` 中 `capacity` 调整）
5. scheduler 是否启用 `DynamicResourceAllocation` 与 `DRAConsumableCapacity`

### ResourceSlice 无设备 / `0 allocatable device(s)`

- 节点上存在**未跟踪**的残留 vHCU（`VDeviceCount != 0` 且不在 prepared 状态中）：驱动跳过该卡。清理残留 vHCU 或重启驱动（启动/周期任务会销毁无 Claim 跟踪的 vHCU）。已 Prepare 的 vHCU 所在物理卡会继续发布，以便同卡 consumable 共享
- DCGM 初始化失败：检查驱动日志与节点 HCU 驱动状态

### Pod Running 但容器内无设备

1. 确认 containerd/CRI-O 已启用 CDI 且 `cdi_spec_dirs` 含 `/var/run/cdi`
2. 检查节点 `/var/run/cdi/hcu-dra-driver-*.yaml` 是否存在
3. 查看驱动 Prepare 日志：`kubectl -n kube-system logs ds/hcu-dra-driver --tail=300`
4. 未开 CDI 时改用 `pod-hcu-consumer-manual-hostmounts.yaml`

### `DRA driver dra.hygon.com is not registered`

- kubelet `--root-dir` 与 DaemonSet 中 `plugins_registry` / `plugins` hostPath 不一致
- 驱动 Pod 未在目标节点 Running（检查 `hcu=on` 标签）

### `some fields were dropped ... DRAConsumableCapacity`

- apiserver、scheduler、kubelet 未全部启用 `DRAConsumableCapacity`
- 临时规避：Claim 去掉 `exactly.capacity`；根治：全链路开门控并滚动更新驱动

### `getCardAndRender failed ... no matching drm dir`

sysfs 下 DRM 路径与 PCI 不匹配。CDI 仍会挂载 `/dev/kfd` 与 hyhal；若业务强依赖 `/dev/dri/*`，在节点确认 `ls /sys/module/*/drivers/pci:*` 下对应 PCI 的 `drm` 目录。

## 项目结构

```
k8s-hcu-dra-driver/
├── cmd/hcu-dra-driver/         # 可执行入口（仅 main）
│   └── main.go
├── bin/                        # 构建产物：hcu-dra-driver（gitignore）
├── dist/                       # 镜像 tar 导出目录（gitignore）
├── internal/driver/            # DRA 驱动核心逻辑（私有包）
│   ├── driver.go               # Prepare/Unprepare 入口
│   ├── driver_prepare.go       # Prepare 实现
│   ├── device_state.go         # 设备状态与初始化
│   ├── allocatable_device.go   # ResourceSlice 设备与容量
│   ├── device_enum.go          # DCGM 枚举与 vHCU 残留清理
│   ├── cdi_spec.go             # CDI spec 生成
│   ├── vdev_dynamic.go         # 动态 vHCU 创建/销毁
│   ├── state_persistence.go    # Prepare 状态持久化
│   └── ...
├── deployments/
│   ├── static/                 # DaemonSet、DeviceClass、RBAC
│   └── examples/               # ResourceClaim 与 Pod 示例
├── scripts/
│   └── build.sh                # 构建脚本（binary / image / tar / all）
├── build.sh                    # 调用 scripts/build.sh 的包装脚本
├── Dockerfile
├── .dockerignore
├── go.mod
├── LICENSE
├── NOTICE
├── THIRD_PARTY_NOTICES.md
└── README.md
```

## 清理测试资源

```bash
kubectl -n hcu-dra-test delete pod --all --ignore-not-found
kubectl -n hcu-dra-test delete resourceclaim --all --ignore-not-found
kubectl delete ns hcu-dra-test --ignore-not-found
```

## License

本项目名称：**k8s-hcu-dra-driver**（Hygon HCU DRA Driver）。

本项目基于 [Apache License 2.0](LICENSE)（`Apache-2.0` / SPDX）开源。

Copyright (c) 2026 Hygon Information Technology Co., Ltd.

- 许可证全文：[LICENSE](LICENSE)
- 版权声明：[NOTICE](NOTICE)

## Third-Party

第三方依赖与来源清单见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。本仓库不内嵌 `vendor/` / `third_party/` 源码副本；Go 依赖通过 Modules 拉取，明细与许可证以该清单为准。
