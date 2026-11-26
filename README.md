# terraform-provider-mgcl

OpenTofu provider，把 [mgcl](https://github.com/geektr-cloud/openmgcl) 中的 bootstrap secret 提供给 OpenTofu 配置：

- `mgcl_secret` ephemeral resource：读取一个 Secret 的全部 KV（public + ephemeral），值只存在于 provider 进程内存中，由 OpenTofu 核心保证不会写入 plan/state。
- `mgcl_secret` managed resource：以 `contents` map 声明式管理一个 Secret 的一组 KV，一次原子批量写入；value 始终进入 plan/state，`Sensitive` 只控制 CLI 展示。
- `provider::mgcl::secret_value(reference)` provider function：读取 public KV，返回普通 string 或 `map(string)`。**结果会进入 plan/state/log。**
- `provider::mgcl::i_just_fucking_wanna_read_ephemeral(reference)` provider function：读取 public + ephemeral KV，返回普通 string 或 `map(string)`。函数名中的 `ephemeral` 不赋予 OpenTofu ephemeral 语义；**结果同样会进入 plan/state/log。**
- `mgcl_secret_kv` managed resource：写入一个 KV；value 始终进入 plan/state，`Sensitive` 只控制 CLI 展示。

同一个名字可以同时作为 `mgcl_secret` ephemeral resource 和 `mgcl_secret` managed resource：它们是不同类别，不共享资源身份。不要在同一项目里用 ephemeral/function 读、又用 managed resource 写同一个 KV。

Provider 不注册 data source。

> v0.2.4 已通过 GitHub Releases 发布，但尚未发布到 OpenTofu Registry。可从 GitHub Release 下载、用 `go install github.com/geektr-cloud/terraform-provider-mgcl@v0.2.4` 安装，或通过 `dev_overrides` / filesystem mirror 使用，详见下文。v0.1.0 仅包含 `mgcl_secret` ephemeral resource；`mgcl_secret_kv` 和单参数 dynamic `provider::mgcl::secret_value` 从 v0.2.0 开始提供；managed `mgcl_secret`（含批量 API 与分层 Secret name）从 v0.2.2 开始提供；Secret name segment 与 KV key 的 PathEscape 校验从 v0.2.3 开始提供；`provider::mgcl::i_just_fucking_wanna_read_ephemeral` 从 v0.2.4 开始提供。

> **public 不等于明文存储。** public KV 仍在 `mgcl.db` 中加密保存，读取仍需私钥和 provider token；但 function 结果一旦被 OpenTofu 求值，就是普通数据，可能被写进 plan 文件、state、output 和日志。不要把真正的机密 KV 标记为 public。

> **`mgcl_secret` / `mgcl_secret_kv` 的 value 一定会持久化。** 即使是 sensitive，它仍完整存在于 plan 和 state；`Sensitive` 只让 CLI 输出显示 `(sensitive value)`。state 本身必须按敏感文件管理。

## 要求

- **OpenTofu >= 1.11**（ephemeral resource 所需版本），provider source address 为 `registry.opentofu.org/geektr-cloud/mgcl`。
- Terraform 1.10+ 在语言层面支持 ephemeral resources，但本 provider 只在 OpenTofu >= 1.11 上测试，未发布到 Terraform Registry，也不以 Terraform 作为支持目标。
- **Go >= 1.27.4**（`go.mod` 的 `go` 指令）。
- 只支持在 `mgcl run -- <command>` 启动的子进程中运行。常驻 `mgcl run` 不签发 provider token。

## 工作方式

Provider address：`registry.opentofu.org/geektr-cloud/mgcl`
二进制名：`terraform-provider-mgcl`（release zip 内为 `terraform-provider-mgcl_v<version>`，符合 OpenTofu 的版本后缀惯例）

`provider "mgcl" {}` 没有任何 HCL 参数。连接信息只从环境变量读取，token 不允许写入配置文件或 HCL：

| 环境变量 | 含义 |
|---|---|
| `MGCL_PROVIDER_ENDPOINT` | provider API 的 base URL，例如 `http://127.0.0.1:1217`，由 `mgcl run --` 按实际 listener 注入 |
| `MGCL_PROVIDER_TOKEN` | 仅限本次子进程生命周期的 provider token，由 `mgcl run --` 注入 |

典型流程：

```bash
# 可选：dev.tfrc 放在 ~/.tofurc 时无需设置
export TF_CLI_CONFIG_FILE=/path/to/dev.tfrc
mgcl run --db ./mgcl.db -- tofu plan
```

`mgcl run --` 未设置 `TF_CLI_CONFIG_FILE` 时自动读取并合并默认的 `~/.tofurc`（存在时）；显式设置时使用指定文件，覆盖默认 `~/.tofurc`。合并结果写入临时 CLI config 传给子进程。

`mgcl run --` 只注入上面两个变量和既有的 `TF_*` 变量，不注入 Secret value。Secret value 由 provider 在 Open、function 调用或 resource 操作时通过 provider API 读取或写入。

Provider token 是短期令牌，只能访问 provider API：读取 KV、写入 KV、批量写入/删除 KV、幂等删除 KV；不能访问 TACOS state、workspace、lock 或其他用户 API。每个 KV 有 access mark：`ephemeral` 或 `public`。读取时 `access: "ephemeral"` 返回全部 KV 与 marks，`access: "public"` 只选择和返回 public KV。Secret 和 KV 需先用 `mgcl secret` 离线准备好；`mgcl secret set <name>/<key> --public` 把该 KV 标记为 public。`mgcl_secret` managed resource 创建 Secret 时由服务端原子完成。

### Secret 名称、KV key 与引用

- **Secret name 可以分层**：一个或多个 `/` 分隔的非空 segment；每段必须满足 `segment == url.PathEscape(segment)`，整串长度（含 `/`）不超过 64 字节。大小写、`.`、`..`、前导 `-`/`_` 和其他 PathEscape-stable 字符都合法，例如 `net.sxxfuture.infra` 和 `aws-605592676484-cn/scarborough`；会被 `url.PathEscape` 编码的字符（例如空格、`%`、`?`、`#` 和非 ASCII）非法。
- **KV key 是单个 segment**：非空、最多 64 字节且 `key == url.PathEscape(key)`，因此不能包含 `/`；大小写、`.` 和其他 PathEscape-stable 字符合法，会被 `url.PathEscape` 编码的字符非法。分层只属于 Secret name。
- **`<secret-name>/<key>` 引用按最后一个 `/` 切分**：`provider::mgcl::secret_value`、`mgcl_secret_kv.key` 都遵循该规则；function 的 wildcard 写作 `<secret-name>/*`。`url.PathEscape("*") == "%2A"`，因此 `*` 不会成为合法的 name segment 或 KV key，不会与 wildcard 冲突。
- managed `mgcl_secret` 的 `name` 使用完整 Secret name；`contents` 的 map key 是裸 KV key，不能再带 Secret name 或 `/`。
- ephemeral `mgcl_secret` 的 `name` 同样接受分层名称。

## HCL 示例

### ephemeral resource（全部 KV）

```hcl
terraform {
  required_providers {
    mgcl = {
      source = "geektr-cloud/mgcl"
    }
  }
}

provider "mgcl" {}

ephemeral "mgcl_secret" "bootstrap" {
  name = "bootstrap"
}

provider "aws" {
  region     = "us-east-1"
  access_key = ephemeral.mgcl_secret.bootstrap.values["access_key_id"]
  secret_key = ephemeral.mgcl_secret.bootstrap.values["secret_access_key"]
}
```

`values` 是 `map(string)` 且标记为 sensitive，包含该 Secret 的 public 与 ephemeral KV。需要多次引用时可用 locals 简化（ephemeral 属性会保持 ephemeral 语义）：

```hcl
locals {
  bootstrap = ephemeral.mgcl_secret.bootstrap.values
}
```

### public provider function

单 KV：

```bash
mgcl secret create app-config --db ./mgcl.db --encryption-key-file ~/.ssh/id_ed25519
mgcl secret set app-config/region us-east-1 --public --db ./mgcl.db --encryption-key-file ~/.ssh/id_ed25519
```

```hcl
resource "terraform_data" "region" {
  input = provider::mgcl::secret_value("app-config/region")
}
```

整个 public KV 集合：

```hcl
resource "terraform_data" "app_config" {
  input = provider::mgcl::secret_value("app-config/*")
}
```

`reference` 是 required string，只接受两种形式：

- `<secret-name>/<key>`：该 KV 必须 public，返回 `string`。
- `<secret-name>/*`：返回该 Secret 的全部 public KV，类型 `map(string)`；没有 public KV 时返回非 nil 空 map。

`reference` 按最后一个 `/` 切分：Secret name 可以包含多个 `/` segment（每段非空且满足 `segment == url.PathEscape(segment)`，整串不超过 64 字节），key 是最后一个非空 segment（最多 64 字节且 `key == url.PathEscape(key)`，因此不含 `/`）。大小写、`.`、`..`、前导 `-`/`_` 等 PathEscape-stable 字符合法；会被 `url.PathEscape` 编码的字符（例如空格、`%`、`?`、`#` 和非 ASCII）、空 segment/key 和 wildcard name 在请求前报错。ephemeral KV 不在 public 集合中，单 KV 形式读取会报 `was not found among the public KVs`。

**函数结果不是 sensitive/ephemeral，OpenTofu 会像普通数据一样持久化它。** 适用于可公开或可落盘的非机密配置；真正的机密继续使用 `mgcl_secret` ephemeral resource。

同一个 provider 进程内，函数按 Secret name 缓存整份 public values map：同 Secret 的不同 key 只触发一次 `secrets:read`，命中缓存不再 HTTP 或解密。缓存只存在于当前 provider 进程，新的 `tofu` / `mgcl run` 会重新读取；OpenTofu 在一次命令中可能启动多个 provider 进程，因此从 API 侧可能看到每个进程各一次请求。ephemeral resource 不使用该缓存，每次 Open 都重新请求。

> **不要在同一个 OpenTofu 项目里读写同一个 KV。** managed `mgcl_secret` / `mgcl_secret_kv` 的写入不会使 function 缓存失效；本 provider 不承诺写入与读取之间的幂等性、事务顺序或最终一致性。需要写入后再读取时，用独立项目分阶段 apply。

### all-KV provider function（public + ephemeral）

单 KV（public 或 ephemeral 都可）：

```hcl
resource "terraform_data" "token" {
  input = provider::mgcl::i_just_fucking_wanna_read_ephemeral("app-config/token")
}
```

整个 KV 集合：

```hcl
resource "terraform_data" "app_config" {
  input = provider::mgcl::i_just_fucking_wanna_read_ephemeral("app-config/*")
}
```

`reference` 的语法、required string 约束、按最后一个 `/` 切分以及 name/key 的 PathEscape 校验与 `provider::mgcl::secret_value` 完全一致：

- `<secret-name>/<key>`：返回该 KV 的 `string`，不区分 public 或 ephemeral。
- `<secret-name>/*`：返回该 Secret 的全部 KV（public + ephemeral），类型 `map(string)`；空 Secret 返回非 nil 空 map。
- null、unknown 和非法格式在请求前报错；Key 不存在时报 `was not found in mgcl secret`，不会误称 public-only。

该函数通过 `secrets:read` 发送 `access: "ephemeral"`。响应仍 fail closed：返回的 Secret name 必须匹配，每个返回 KV 必须有显式 `ephemeral_marks` 元数据（`true`/`false` 都合法）；任一 KV 缺 mark 时整次报错且不缓存。

**函数名不代表 OpenTofu ephemeral 语义。** 返回值是普通、非 sensitive 的动态值，public 和 ephemeral KV 都可能进入 plan、state、output 和日志。如果某个值必须永不落盘，使用 `mgcl_secret` ephemeral resource，而不是这个 function。`provider::mgcl::secret_value` 仍然只读 public KV，合同不变。

该函数在每个 provider 进程内按 Secret name 缓存整份 all-KV map，与 `secret_value` 的 public cache 是两个独立 namespace：public 读永远不会看到 all cache 中的 ephemeral 值，all 读也不会满足 public 读。两边都只缓存成功且名称匹配的响应，失败不缓存，并发 miss 合并为一次请求，返回副本；managed `mgcl_secret` / `mgcl_secret_kv` 的写入同样不会使该缓存失效。

### managed resource：整个 Secret 的 KV 集合

```hcl
resource "mgcl_secret" "scarborough_cred" {
  name = "aws-605592676484-cn/scarborough"
  contents = {
    account_id = { value = provider::mgcl::secret_value("aws-605592676484-cn/account_id"), ephemeral_mark = false }
    region     = { value = provider::mgcl::secret_value("aws-605592676484-cn/region"), ephemeral_mark = false }
    access_key = { value = module.service_scarborough_root.access_key, ephemeral_mark = false }
    secret_key = { value = module.service_scarborough_root.secret_key }
    role_arn   = { value = module.service_scarborough_root.role_arn, ephemeral_mark = false }
  }
}
```

- `name`：required，完整 Secret name，可以包含多个 `/` segment；整串不超过 64 字节。修改 `name` 会替换资源。
- `contents`：required `MapNestedAttribute`；map key 是裸 KV key（单个非空 segment，最多 64 字节且 `key == url.PathEscape(key)`，不能含 `/`），value 是包含 `value` 和 `ephemeral_mark` 的对象。允许空 map `{}`（确保 Secret 存在但没有 KV），不允许 `null`。
- entry `value`：required string，`Sensitive: true`，但仍完整进入 plan/state；空字符串允许，`null` 拒绝。validate 阶段跳过 unknown，Create/Update 要求 name、所有 key 和 value 已知。
- entry `ephemeral_mark`：optional bool，默认 `true`。`true` 的 KV 不能被 `provider::mgcl::secret_value` 读取；ephemeral `mgcl_secret` 会返回它。
- Create/Update 把整个 `contents` 和需要删除的旧 key 合并为**一次原子** `secret-kvs:write-batch` 请求；Create 的 `delete_keys` 为空。Secret 不存在时由服务端连同 DEK 原子创建。只拥有 `contents` 中的 key：远端额外 KV 不动，删除时也不动。
- 同一个 KV 只能由一个资源拥有：`mgcl_secret` managed 与 `mgcl_secret_kv` 之间、两个 managed 资源之间都不能管理同一个 Secret 的同一个 key。不同资源管理**同一 Secret 的 disjoint keys** 可以；重叠 ownership 不受支持。
- plan 本地预检：`contents` 最多 64 个 entry，单个 value ≤ 64 KiB，plan values 合计 ≤ 256 KiB（只统计 plan values）；超限在发请求前报错。服务端仍按最终 KV 集合校验。
- Update 的 `delete_keys` 等于旧 state 有、plan 没有的 key；`value`/`ephemeral_mark` 变化原地更新。
- 响应只用于逐项核对 name、key 和 mark；响应不含 value，provider 不采用任何 value。名称、key 集合或 mark 不匹配时报 `Unexpected mgcl secret response`。
- `Read` 用 `secrets:read`（access `ephemeral`）读取全部 KV 和 marks。Secret 404 时从 state 移除资源；某个 managed key 缺失时只从 state `contents` 删除该 key，让下一次 plan/update 补回；存在的 managed key 同步 value/mark 以发现 drift；额外 KV 忽略；managed key 缺 mark 时安全报错，不猜测。
- `Delete` 用同一次批量请求（`ensure=false`）删除 state 中的 key。批量端点对缺失 Secret/KV 返回 200，属幂等成功，绝不创建 Secret；HTTP 404 表示当前 mgcl 不支持批量端点，会报错而不是静默丢弃 state。永不删除 Secret、DEK 或额外 KV。空 `contents` 不发请求。不支持 import。
- 不要在同一个 OpenTofu 项目里用 function/ephemeral 读、又用该资源写同一个 KV；provider 不做 function cache invalidation，也不承诺读写顺序。

### managed resource：单个 KV

```hcl
resource "mgcl_secret_kv" "region" {
  key            = "app-config/region"
  value          = "us-east-1"
  ephemeral_mark = false # optional, default true
}
```

- `key`：required，`<secret-name>/<key>`，按最后一个 `/` 切分；Secret name 可分层，key 必须是单 segment。name/key 非法时在 plan 阶段报错。修改 `key` 会替换资源（destroy 旧 KV，create 新 KV）。
- `value`：required，`Sensitive: true`，但仍完整进入 plan/state。
- `ephemeral_mark`：optional bool，默认 `true`。`true` 的 KV 不能被 `provider::mgcl::secret_value` 读取；`mgcl_secret` 会返回它。修改 value/mark 原地 update。
- Secret 不存在时由 provider API 原子创建；删除 KV 后 Secret 保留，可能为空。不实现 import。
- 同一个 KV 只能由一个资源拥有；`mgcl_secret_kv` 与 managed `mgcl_secret` 之间不能重叠。不同资源可以管理同一 Secret 的 disjoint keys。

`Read` 会重新读取 Secret 的全部 KV 和 marks，比较 value 与 mark 以发现 drift。Secret 或 KV 不存在时从 state 移除资源；响应缺少 mark 时直接报错，不猜测。

## 安装

### 方式一：GitHub Releases

从 [Releases](https://github.com/geektr-cloud/terraform-provider-mgcl/releases) 下载对应平台的 zip：

```text
terraform-provider-mgcl_<version>_<os>_<arch>.zip
```

每个 release 同时附带 `terraform-provider-mgcl_<version>_SHA256SUMS`、它的 detached 二进制 GPG 签名 `terraform-provider-mgcl_<version>_SHA256SUMS.sig`，以及 `terraform-provider-mgcl_<version>_manifest.json`。解压后放到 OpenTofu 可读取的位置：`dev_overrides` 目录，或 filesystem mirror 布局（见后文）。

GitHub Release 资产**不会**自动加入 OpenTofu Registry。没有 `dev_overrides` / filesystem mirror 配置时，OpenTofu 无法从 Registry 安装本 provider。

### 方式二：go install

```bash
go install github.com/geektr-cloud/terraform-provider-mgcl@v0.2.4
```

二进制安装到 `$(go env GOBIN)`（未设置时为 `$(go env GOPATH)/bin`），文件名为 `terraform-provider-mgcl`。OpenTofu 不会自动发现该路径：把二进制所在目录配置为 `dev_overrides`，或复制到 filesystem mirror 布局后使用。

### 方式三：dev_overrides（快速开发）

`dev_overrides` 让 OpenTofu 直接使用本地二进制，并跳过该 provider 的 `tofu init` 安装与校验。

```bash
go build -o bin/terraform-provider-mgcl .
```

```hcl
# dev.tfrc
provider_installation {
  dev_overrides {
    "geektr-cloud/mgcl" = "/home/me/src/terraform-provider-mgcl/bin"
  }
  direct {}
}
```

```bash
# 可选：dev.tfrc 就是 ~/.tofurc 时无需设置
export TF_CLI_CONFIG_FILE=/home/me/src/terraform-provider-mgcl/dev.tfrc
tofu plan
```

`dev_overrides` 的路径必须指向**包含 `terraform-provider-mgcl` 二进制的目录**（上面示例的 `bin/`），不是二进制文件本身。

`TF_CLI_CONFIG_FILE` 可选：未设置时 `mgcl run --` 自动继承 `~/.tofurc`；显式设置时以该变量为准，覆盖默认 `~/.tofurc`。`dev_overrides` 会跳过 `tofu init`；需要真正执行 `tofu init` 时改用方式四。

### 方式四：filesystem mirror（需要 tofu init）

需要走 `tofu init` 的集成场景使用符合 OpenTofu 布局的 filesystem mirror。最小 unpacked 布局：

```text
/opt/tofu-mirror/registry.opentofu.org/geektr-cloud/mgcl/0.2.4/linux_amd64/terraform-provider-mgcl_v0.2.4
```

```hcl
# dev.tfrc
provider_installation {
  filesystem_mirror {
    path    = "/opt/tofu-mirror"
    include = ["registry.opentofu.org/geektr-cloud/mgcl"]
  }
  direct {
    exclude = ["registry.opentofu.org/geektr-cloud/mgcl"]
  }
}
```

```bash
# dev.tfrc 不在 ~/.tofurc 时才需要显式设置
export TF_CLI_CONFIG_FILE=/opt/tofu-mirror/dev.tfrc
tofu init
```

与 `dev_overrides` 不同，filesystem mirror 支持真实 `tofu init`。`mgcl run --` 未设置 `TF_CLI_CONFIG_FILE` 时同样继承 `~/.tofurc`；显式设置时覆盖默认文件。

也支持 packed 布局：`.../mgcl/terraform-provider-mgcl_0.2.4_linux_amd64.zip`。

## 行为与保证

- `provider "mgcl" {}` 无必填参数。`Configure` 只读环境变量，不访问网络，并把同一份 provider data 交给 ephemeral resource、managed resource 和 provider function。**不含 `provider::mgcl::secret_value` / `provider::mgcl::i_just_fucking_wanna_read_ephemeral` 的配置**在缺少 endpoint/token 时也能通过 `tofu validate`：validate 只检查 schema 与本地格式，不出网；第一次 Open 或 resource 操作才报缺少配置。**含 provider function 的配置**在 validate 阶段就会求值 function，因此需要真实的 provider session（`mgcl run --` 注入的 endpoint/token），否则 validate 直接报未配置。OpenTofu 可能在 `Configure` 之前求值 provider function，此时函数直接从环境变量初始化，不发网络请求。
- `mgcl_secret` ephemeral 的 `name` 为必填 string：一个或多个非空 `/` segment，每段满足 `segment == url.PathEscape(segment)`，整串不超过 64 字节；大小写、`.`、`..`、前导 `-`/`_` 等 PathEscape-stable 字符合法。`tofu validate` 在 name 已知时检查语法，unknown 时跳过；Open 对 null 返回 `Secret name is required`，对 unknown 返回 `Secret name must be known`。
- managed `mgcl_secret` 的 `name` 规则与 ephemeral 相同且变更触发 replace；`contents` required，空 map 合法、null 拒绝；每个 entry 的 `value` required 且 sensitive，`ephemeral_mark` optional+computed、默认 true。`contents` key 必须是单 segment（不能含 `/`）。validate 跳过 unknown；Create/Update 要求 name、key、value 均 known，null value 拒绝，空 value 允许。
- `provider::mgcl::secret_value` 和 `provider::mgcl::i_just_fucking_wanna_read_ephemeral` 的 `reference` 都是 required string。null、unknown、非法格式在请求前报错；两者共用同一套解析与 PathEscape 校验。
- `mgcl_secret_kv` 的 `key` 为 required string，plan 阶段校验；`value` required；`ephemeral_mark` 默认 true。key 变更触发 replace，value/mark 变更原地 update。
- provider API 响应的 `name` 必须与请求一致；ephemeral resource、function 和 managed resource 都会丢弃名称不匹配的返回值并报错，不采用任何 value。name 不匹配诊断会 redact provider token。
- `mgcl_secret` 每次 Open 调用一次 `secrets:read`（access `ephemeral`），返回全部 KV 与 marks，不跨 Open 缓存。plan 与 apply 各自 Open，因此两次之间更新 Secret 后会读到新值。
- `provider::mgcl::secret_value` 只调用 `secrets:read` 且 access 为 `public`，返回 public KV。函数在每个 provider 进程内按 Secret name 缓存整份 public values map，带 mutex；同 Secret 不同 key、并发 miss 都只请求一次。只缓存成功、名称匹配且满足 per-KV mark 合同的响应：每个返回 key 必须有显式 `false` mark；缺失 mark、`true` mark 或混合非法响应整体报错，且不缓存、不返回任何 value。空响应（values 与 marks 都缺失或为空）是合法空 public 集，归一为非 nil 空 map；返回副本，调用方不能修改缓存。不同 Secret 分别请求。
- `provider::mgcl::i_just_fucking_wanna_read_ephemeral` 只调用 `secrets:read` 且 access 为 `ephemeral`，返回 public + ephemeral KV。它使用与 public cache 独立的 all-KV cache namespace，缓存规则相同：同 Secret 不同 key、并发 miss 都只请求一次；只缓存成功、名称匹配且每个返回 key 都有显式 `ephemeral_marks` 的响应（`true`/`false` 都合法），缺 mark 时整体报错且不缓存；空响应是合法空集，归一为非 nil 空 map；返回副本。public 与 all 两个 cache 不共享条目，任何方向的读取都不会互相满足或泄漏。
- 函数缓存只存活当前 provider 进程，新的 `tofu` / `mgcl run` 重新读取。OpenTofu 在一次命令中可能启动多个 provider 进程，因此服务端可能看到每个进程各一次请求。managed `mgcl_secret` / `mgcl_secret_kv` 写入不会使任一个函数缓存失效；不要在同一项目里读写同一 KV。
- `provider::mgcl::secret_value` 返回普通非 sensitive 数据，可被 plan/state/log 持久化。这是使用 public KV 的明确结果；KV 本身仍在 `mgcl.db` 中加密保存。
- `provider::mgcl::i_just_fucking_wanna_read_ephemeral` 同样返回普通非 sensitive 数据，public 与 ephemeral KV 都可能被 plan/state/log 持久化。函数名中的 `ephemeral` 不改变 OpenTofu 的类型或持久化语义；必须避免落盘时使用 `mgcl_secret` ephemeral resource。
- managed `mgcl_secret` 通过一次 `secret-kvs:write-batch` 原子应用整个 `contents` 与 `delete_keys`（`ensure=true`），Create 不删除任何 key；Update 的 `delete_keys` 是旧 state 有而 plan 没有的 key。plan 本地预检 64 个 entry、单 value 64 KiB、合计 256 KiB（只统计 plan values），超限零请求。Read 用 `secrets:read` 同步存在的 managed value/mark，缺失的 managed key 只从 state 删除、不删除远端额外 KV；Delete 用同一批量端点（`ensure=false`）幂等删除 state 中的 key，绝不创建或删除 Secret/DEK；批量端点对缺失 Secret 返回 200，404 表示当前 mgcl 不支持该端点，会报错。响应逐项核对 name、key 和 mark，不采用 value。
- `mgcl_secret_kv` 通过 `secret-kvs:write` 创建/覆盖 KV，通过 `secret-kvs:delete` 幂等删除 KV。Create/Update 不采用响应中的 value（响应本来也不含 value）；只校验响应里的 name、裸 key 和 mark。Read 用 `secrets:read` 同步 value/mark；Delete 不删除 Secret。
- managed `mgcl_secret` 和 `mgcl_secret_kv` 的 value 始终持久化在 plan/state；`Sensitive` 只控制展示。state 必须按敏感文件管理。
- 错误诊断不包含 provider token 或 Secret value；404、响应名称不匹配、public 读取的 per-KV mark 违约、all-KV 读取缺 mark、批量响应 key/mark 不匹配、key 缺失和 mark 缺失都有独立诊断，其他 HTTP 状态走通用读写失败诊断。
- `resource "mgcl_secret"` 是合法 managed resource；不要把它与同名 ephemeral resource 混淆。把 ephemeral 值用于会持久化的普通参数时，由 OpenTofu 核心拒绝。

## 排查

| 诊断 | 原因 |
|---|---|
| `MGCL_PROVIDER_ENDPOINT is not set` | 未在 `mgcl run -- <command>` 下运行，或该变量被覆盖 |
| `MGCL_PROVIDER_TOKEN is not set` | 同上；常驻 `mgcl run` 不签发 token |
| `Secret name is required` | `mgcl_secret.name` 为 null |
| `Secret name must be known` | name 依赖 plan 时未知的表达式；改为已知字面量或 plan 期间可知的值 |
| `Invalid secret name` | name 不是合法的分层 Secret name：空、含空 segment、某 segment 不等于 `url.PathEscape(segment)`（例如含空格、`%`、`?`、`#` 或非 ASCII），或整串超过 64 字节 |
| `Invalid secret reference` | function 参数不是 `<secret-name>/<key>` 或 `<secret-name>/*`，或分层 name/单段 key 非法 |
| `Invalid secret KV reference` | `mgcl_secret_kv.key` 按最后一个 `/` 切分后 name/key 非法，或使用了 wildcard |
| `Invalid secret key` | managed `mgcl_secret.contents` 的 map key 为空、超过 64 字节或 `key != url.PathEscape(key)`（含 `/`、空格、`%`、`?`、`#` 或非 ASCII） |
| `Secret contents are required` | managed `mgcl_secret.contents` 为 null；空 map 请写 `{}` |
| `Secret contents must be known` | `contents` 依赖 plan/apply 期间未知的表达式 |
| `Secret value must not be null` | 某个 entry 的 `value = null`；空值请用 `""` |
| `Secret value must be known` | 某个 entry 的 value 在 apply 时仍未知 |
| `Too many Secret contents entries` | plan 的 `contents` 超过 64 个 entry；plan 本地预检，零请求 |
| `Secret value exceeds size limit` | 单个 value 超过 64 KiB；plan 本地预检，零请求 |
| `Secret contents exceed size limit` | plan values 合计超过 256 KiB；plan 本地预检，零请求 |
| `Secret key "..." was not found among the public KVs` | KV 不存在，或它是 ephemeral mark；`provider::mgcl::secret_value` 只读 public 集合 |
| `Secret key "..." was not found in mgcl secret "..."` | KV 不存在；`provider::mgcl::i_just_fucking_wanna_read_ephemeral` 读取 public + ephemeral 集合 |
| `Failed to read public KVs` | `secrets:read`（access `public`）失败，例如 401/403/5xx；详情见诊断 |
| `Failed to read KVs` | `secrets:read`（access `ephemeral`）失败，例如 401/403/5xx；详情见诊断 |
| `mgcl secret "..." was not found; its public KV set could not be read` | `provider::mgcl::secret_value` 请求的 Secret 不存在 |
| `mgcl secret "..." was not found; its KV set could not be read` | `provider::mgcl::i_just_fucking_wanna_read_ephemeral` 请求的 Secret 不存在 |
| `Missing ephemeral mark in mgcl secret response` | Read 返回了 managed value 却没有对应 mark；provider 拒绝猜测 |
| `Unexpected mgcl secret response` | provider API 返回了不同 Secret 名称、public 读取违反 per-KV public mark 合同、all-KV 读取缺少 per-KV mark、批量响应 key/mark 不匹配、空响应或不匹配的写入元数据；返回值已被丢弃 |
| `Failed to read mgcl secret` | ephemeral Open 或 managed resource Read 遇到 401/404/服务端错误；详情见诊断 |
| `Failed to write mgcl secret` | managed `mgcl_secret` 的 `secret-kvs:write-batch` 失败；400/404 的详情会提示可能需要支持该端点和层级 name 的新版 mgcl |
| `Failed to delete mgcl secret KVs` | managed `mgcl_secret` 的批量删除失败；404 表示当前 mgcl 不支持批量端点，不会当作幂等成功 |
| `Failed to write mgcl secret KV` | `secret-kvs:write` 失败；详情见诊断 |
| `Failed to delete mgcl secret KV` | `secret-kvs:delete` 失败；详情见诊断 |

## 开发

```text
main.go                        providerserver 入口，address registry.opentofu.org/geektr-cloud/mgcl
internal/provider/provider.go  provider Metadata/Schema/Configure，共享 providerData 与 function 缓存
internal/provider/secret_ephemeral.go  mgcl_secret ephemeral 的 Schema/ValidateConfig/Open/Close 与分层名称校验
internal/provider/secret_resource.go  mgcl_secret managed 的 Schema/Create/Read/Update/Delete（批量写入）
internal/provider/secret_value_function.go  secret_value 与 i_just_fucking_wanna_read_ephemeral 的共享解析、Definition/Run
internal/provider/secret_kv_resource.go  mgcl_secret_kv 的 Schema/Create/Read/Update/Delete
internal/provider/secret_cache.go  public 与 all-KV 的独立进程内缓存及响应校验
e2e/                           真实 OpenTofu e2e（需要 PATH 中有 tofu >= 1.11）
```

文档由 `tfplugindocs` 从 provider schema 与 `examples/` 生成到 `docs/`：

```bash
make generate
```

`make generate` 在独立 `tools` Go module 中运行 `tofu fmt -recursive examples/`，再执行 `tfplugindocs generate` 与 `validate`。修改 schema、资源/function 注册、描述或 `examples/` 后必须重新生成并提交 `docs/`；`docs/` 是提交物，不要手改生成文件。工具依赖固定在 `tools/go.mod`，不会进入主 `go.mod`。

校验：

```bash
gofmt -l .
go vet ./...
go test ./... -count=1
go test -race -short ./... -count=1
go build ./...
go mod verify
go mod tidy -diff
make generate
(cd tools && go mod verify && go mod tidy -diff)
git diff --check
```

CI（`.github/workflows/ci.yml`）在 push `main`、pull request 和手动触发时运行，矩阵覆盖 OpenTofu `1.11.11` 与 `1.13.9`，执行 gofmt 检查、`go test ./... -count=1`、`go test -race -short ./... -count=1`、`go vet ./...`、`go build ./...`、`go mod verify`。OpenTofu `1.13.9` 的 job 额外安装 Terraform `1.17.2` 并运行 `make generate`，再用 `git diff --exit-code` 与 `git status --porcelain` 确认生成文档已提交且内容最新；untracked 的新文档同样会使检查失败。

CI 和 Release 都只 checkout 本仓，在仓库根目录运行所有 Go 命令，GoReleaser 也从根目录构建。OpenTofu provider 依赖的 `github.com/geektr-cloud/openmgcl v0.2.3` 由 Go module proxy 或 module cache 提供，不需要同级 checkout。Release 由 `v*` tag 触发，checkout 该 tag 并从中构建。

发布由 `.github/workflows/release.yml` 驱动：推送 `v*` tag 后先安装 Terraform `1.17.2`、运行 `make generate` 并检查 `docs/` 无差异（生成文档过期即失败），再运行完整 `go test ./... -count=1` 与 `go vet ./...`，然后用 GoReleaser 构建 linux/darwin/windows × amd64/arm64 的 zip、`SHA256SUMS` 和 Registry manifest，创建 GitHub Release。GitHub Release 不会自动把 provider 加入 OpenTofu Registry。

`e2e/` 有两层测试，都会在缺少 `tofu >= 1.11` 或 `-short` 时明确 skip：

- **hermetic e2e**：用 `httptest` 假 API 验证 dev_overrides、plan/apply、空 Secret、分层名称、function 单 KV 与 wildcard map、public KV 持久化、ephemeral KV 隔离、all-KV function 读取 public+ephemeral 单 KV/wildcard 并持久化、`mgcl_secret` ephemeral 返回混合 KV、managed `mgcl_secret` 的空 map/默认 mark/增删改/响应校验/drift/destroy、同名 managed+ephemeral、function→managed、连续两次 apply 无变化、独立项目读取、`mgcl_secret_kv` 创建/更新/替换/drift/destroy、非法 reference 零请求、误用与 filesystem mirror（unpacked 与 packed）init，不依赖 openmgcl 源码。假 API 实现 `secrets:read`、`secret-kvs:write`、`secret-kvs:write-batch`、`secret-kvs:delete`，按 KV mark 过滤 public 读取，并校验 token。binary plan 文件是 zip+Deflate，泄漏检查会逐 entry 解压后再扫描。
- **跨仓真实 e2e**：通过 `go list -m` 找到 `go.mod` 中解析的 openmgcl 源码（通常来自 Go module cache），把 `cmd/mgcl` 构建到临时目录，用临时 HOME、临时 0600 Ed25519 key 和临时 SQLite DB 执行 `mgcl secret create/set/list/show/get`，再经 `mgcl run --db ... --encryption-key-file ... -- tofu ...` 跑真实 OpenTofu。覆盖：不含 function 的配置在无 session 时 validate 通过、plan/apply 经真实 API 读取、更新 Secret 后新 run 读到新值、空 Secret、`--public` 后 function 读取单 KV 与 wildcard 并持久化 public 值、ephemeral KV 不可由 `secret_value` 读、all-KV function 读取 public+ephemeral 单 KV 与 wildcard 并持久化、混合 Secret 的 `mgcl_secret` 返回全部 KV、`mgcl_secret_kv` 在同 Secret 创建/更新/替换/删除两个 KV、managed `mgcl_secret` 的分层名称/默认 mark/function source→managed target/第二次 apply 无变化/增删改/额外 KV 保留/destroy 保留 Secret、同名 managed+ephemeral、独立项目读取 managed Secret 的 public 与 ephemeral KV、ephemeral 值写入持久化参数被核心拒绝、provider token 跨 session 返回 401、非 loopback 安全门在加载 key/DB/listen 前失败。

所有端口由 listener 临时分配。**在 Go 依赖与工具链已缓存的前提下**，tofu/mgcl 运行期不访问外网；冷缓存时 `go build` 仍可能访问 Go module proxy。

跨仓测试从 `go list -m` 返回的 openmgcl 源码目录构建（通常来自 Go module cache）；构建产物写入临时目录，不写入 module cache。

### 依赖说明

Provider 通过 `github.com/geektr-cloud/openmgcl/pkg/providerapi` 访问 mgcl provider API（stdlib-only 客户端）。

`require` 固定为 `github.com/geektr-cloud/openmgcl v0.2.3`，没有 `replace`。`ReadPublicSecret`、`WriteSecretKV`、`WriteSecretKVs`、`DeleteSecretKV`、per-KV `ephemeral_marks`、`secret-kvs:write-batch`、分层 Secret name 和 PathEscape-stable Secret name/KV key 校验都包含在这个已发布模块中。本仓可脱离同级 openmgcl 目录独立构建，也支持 `go install github.com/geektr-cloud/terraform-provider-mgcl@v0.2.4`。

跨仓真实 e2e 通过 `go list -m` 解析到同一份 openmgcl 源码（通常来自 Go module cache）。测试在临时目录构建 `mgcl` CLI，不写入源码目录或 module cache。

Terraform Plugin Framework 版本见 `go.mod`（v1.19.0）。
