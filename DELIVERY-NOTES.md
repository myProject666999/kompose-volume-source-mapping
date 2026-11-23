# 卷来源映射交付说明 (Volume Source Mapping — Delivery Notes)

本文档记录本次改动的设计决策，包括规格中标注"待确认"的条目及其结论。

## 1. 类型信息在加载阶段保留

- `loadVolumes`（`pkg/loader/compose/compose.go`）现在返回三元组：
  还原出的 `source:target[:ro]` 字符串列表、长语法 tmpfs 列表、以及
  `map[卷字符串]compose类型`。类型映射存入 `ServiceConfig.VolumeTypes`，
  经 `retrieveVolume` → `ParseVols`（新增可选变参，旧调用签名兼容）写入
  `kobject.Volumes.Type`（`"volume"` / `"bind"` / `""`）。
- compose-go 在加载时会把相对绑定路径解析为绝对路径，因此类型映射的键是
  解析后的绝对路径字符串；`ConfigHostPathVolumeSource` 的"相对路径以
  Compose 文件目录为基准"行为不受影响（绝对路径直接透传）。
- 长语法 `type: tmpfs` 的卷不再进入 `ParseVols`（此前会产出畸形字符串），
  而是编码为 `target[:size=<字节>][,mode=<八进制>]` 追加到 `TmpFs` 列表。

## 2. 默认落点与全局选项、标签的优先级

- 无全局选项、无标签时按 Compose 声明类型落点：
  具名卷 → PVC，绑定挂载 → hostPath，tmpfs → `medium: Memory` 的 emptyDir。
  类型未知（非 compose 来源）时退回旧启发式：有 host 路径 → hostPath，否则 PVC。
- 单个已存在文件的绑定挂载仍按既有 `isConfigFile` 逻辑转为 ConfigMap（保留）；
  被跳过的文件（空路径、`.sock`）的警告现在包含服务名与路径。
- `--volumes emptyDir|hostPath|configMap` 仍强制全部卷（保持现有行为）；
  绑定挂载被降级为 emptyDir 时新增警告，说明宿主机目录内容不会出现在 Pod 里。
  `--volumes persistentVolumeClaim`（默认值）不再视为强制，而是"按类型自然落点"。
- `kompose.volume.type` 标签按服务生效，优先级高于全局选项（不变）；
  取值不在 `ValidVolumeSet` 内仍返回错误（不变）。
- **已知限制**（已写入 `docs/user-guide.md`）：同一服务混用具名卷与绑定挂载时，
  标签只能指定一个类型，所有卷都服从该标签。

## 3. 容量三层来源与解析

优先级（高 → 低）：根级卷 `kompose.volume.size` 标签（`getVolumeLabels` 读取）
> `--pvc-request-size`（`ConvertOptions.PVCRequestSize`）> 内置默认值 `100Mi`。
服务级 `kompose.volume.size` 标签的旧行为保留（在根级标签缺失时生效）。

- `CreatePVC` 解析前先 `TrimSpace`：`512Mi `（带空格）可用；`1G` 是合法的
  十进制 SI 量；`1gi` 非法 → 整体报错，错误信息带卷名与服务名
  （`k.CreatePVC failed for volume <卷> in service <服务>`）。

## 4. 访问模式与挂载只读拆分

- 挂载只读（`VolumeMount.ReadOnly`）逻辑不变，仍由 `:ro` 后缀决定。
- PVC 访问模式新增根级卷标签 **`kompose.volume.access-mode`**（采用规格建议名称），
  取值 `rwo`/`rox`/`rwx`/`rwop`（大小写不敏感、容忍空白）；拼写错误 → 报错并带卷名，
  不再静默回退到 ReadWriteOnce。
- **兼容性决策（保持旧输出 + 警告）**：未写标签时，`:ro` 仍把整个 PVC 声明为
  `ReadOnlyMany`，同时打印警告建议使用新标签。如此旧输出逐字不变。
- `setVolumeAccessMode` 对 Compose 来源仍只能收到 `ro`/`rw`（`ParseVolume` 限制），
  `rwx`/`rwop` 只能经新标签表达 —— 这正是新标签存在的意义。

## 5. 共享具名卷的合并规则

- 多个服务共享同一具名卷时只生成一个 PVC（`RemoveDupObjects` 去重，保留首个）。
  新增 `MergeSharedPVCs`（在去重前运行）：所有引用访问模式一致 → 保持；
  不一致 → **取读写并集但保守落地为 ReadWriteOnce** 并警告
  （即只读服务不会阻塞读写服务，也不会静默升级为 ReadWriteMany）。
  只读服务的 `readOnly` 挂载各自保留。
- 输出确定性：服务本已按名称排序处理；新增 `SortPVCsByName` 使 PVC 按名称排序，
  同一输入多次转换逐字一致（有用例固定）。

## 6. tmpfs

- 长语法 `tmpfs.size`（字节数）与短语法 `/run:size=64m,mode=1777` 都转换为
  emptyDir 的 `sizeLimit`。**单位结论：按 Compose 约定的二进制含义**，
  `64m` = 64MiB（`k/m/g` 后缀均为 1024 进制，可带 `b`，大小写不敏感）；
  裸数字为字节数。非法大小 → 警告并忽略（不中断转换）。
- `mode` 无对应字段 → 警告并忽略。
- `read_only` 服务 + tmpfs：emptyDir 挂载不会被标成只读（有用例确认）。

## 7. 绑定挂载边界

- 相对路径：compose-go 加载时已解析为绝对路径（以 Compose 文件目录为基准），
  `ConfigHostPathVolumeSource` 的 join 逻辑对其幂等。
- `~` 与环境变量：经 compose-go 插值/展开后按普通路径处理，无特殊分支。
- 单文件源：维持 `isConfigFile` → ConfigMap 的既有转换；跳过警告现含服务名与路径。
- Windows 盘符路径：复用 `transformer.ParseVolume` 的 `parseWindowsVolume` 分支，
  长语法拆回字符串后的组合有用例固定（`C:\Users\Data:/etc/config:ro` 往返）。
- 源是文件且类型为 hostPath 时**不设置** `type: File`：kompose 无法验证节点上的
  路径类型，设置后可能导致 Pod 无法调度；保持不设置（与现状一致）。

## 8. external 卷与 driver

- 根级 `external: true` 的具名卷：不生成 PVC，Pod 直接引用预期已存在的 PVC
  （`name:` 字段的值，缺省为卷键），并打印提示。
- `driver` / `driver_opts`：识别后在加载阶段按卷名警告（每卷一次），
  不扩展全局诊断机制。

## 9. 命名与 volumes_from

- PVC 命名仍为 `<服务名>-claim<序号>`（匿名卷）与卷名（具名卷），
  `volumes_from` 经 `retrieveVolume` 递归解析的行为不变。
- `kompose.service.name_override` 在卷解析之前生效（`parseResourceName` 与
  标签处理都改的是服务名），因此匿名卷的 PVC 名跟随改名后的服务名
  （用例固定：`renamed-claim0`）。
- 服务分组（`ServiceGroupMode=volume`）下共享具名卷仍只生成一个 PVC
  （用例固定）；组名替换逻辑（`strings.Replace(volumeName, service.Name, name, 1)`）
  未改动。

## 10. 测试

新增用例全部以临时目录最小 Compose 文件 + 内存 `ConvertOptions` 构造，
不触碰集群或真实存储：

- `pkg/transformer/kubernetes/volume_mapping_test.go`：三个基线（具名卷 / 绑定挂载 /
  tmpfs）+ 逐项叠加（全局选项与标签冲突、三层大小与非法取值、访问模式标签与
  `:ro` 兼容、共享卷合并、tmpfs 长短语法与单位换算、相对路径与单文件源、
  external 卷、服务分组、确定性与 PVC 排序）。
- `pkg/loader/compose/compose_test.go`：类型保留、长语法 tmpfs 编码、
  `ParseVols` 类型传播、Windows 盘符往返、external 标记与名称解析。
