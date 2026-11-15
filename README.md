# terraform-provider-mgcl

OpenTofu provider，把 [mgcl](https://github.com/geektr-cloud/openmgcl) 中的 bootstrap secret 提供给 OpenTofu 配置：

- `mgcl_secret` ephemeral resource：读取一个 Secret 的全部 KV（public + ephemeral），值只存在于 provider 进程内存中，由 OpenTofu 核心保证不会写入 plan/state。
- `provider::mgcl::secret_value(reference)` provider function：读取 public KV，返回普通 string 或 `map(string)`。**结果会进入 plan/state/log。**
- `mgcl_secret_kv` managed resource：写入一个 KV；value 始终进入 plan/state，`Sensitive` 只控制 CLI 展示。

Provider 不注册 data source。

> v0.2.1 已通过 GitHub Releases 发布，但尚未发布到 OpenTofu Registry。可从 GitHub Release 下载、用 `go install github.com/geektr-cloud/terraform-provider-mgcl@v0.2.1` 安装，或通过 `dev_overrides` / filesystem mirror 使用，详见下文。v0.1.0 仅包含 `mgcl_secret` ephemeral resource；`mgcl_secret_kv` 和单参数 dynamic `provider::mgcl::secret_value` 从 v0.2.0 开始提供。

> **public 不等于明文存储。** public KV 仍在 `mgcl.db` 中加密保存，读取仍需私钥和 provider token；但 function 结果一旦被 OpenTofu 求值，就是普通数据，可能被写进 plan 文件、state、output 和日志。不要把真正的机密 KV 标记为 public。

> **`mgcl_secret_kv` 的 value 一定会持久化。** 即使是 sensitive，它仍完整存在于 plan 和 state；`Sensitive` 只让 CLI 输出显示 `(sensitive value)`。state 本身必须按敏感文件管理。

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

Provider token 是短期令牌，只能访问 provider API：读取 KV、写入 KV、幂等删除 KV；不能访问 TACOS state、workspace、lock 或其他用户 API。每个 KV 有 access mark：`ephemeral` 或 `public`。读取时 `access: "ephemeral"` 返回全部 KV 与 marks，`access: "public"` 只选择和返回 public KV。Secret 和 KV 需先用 `mgcl secret` 离线准备好；`mgcl secret set <name>/<key> --public` 把该 KV 标记为 public。`mgcl_secret_kv` 创建 Secret 时由服务端原子完成。

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

name 和 key 都必须匹配 `^[a-z0-9][a-z0-9_-]{0,63}$`。其他格式、多个 slash、非法 name/key 在请求前报错。ephemeral KV 不在 public 集合中，单 KV 形式读取会报 `was not found among the public KVs`。

**函数结果不是 sensitive/ephemeral，OpenTofu 会像普通数据一样持久化它。** 适用于可公开或可落盘的非机密配置；真正的机密继续使用 `mgcl_secret` ephemeral resource。

同一个 provider 进程内，函数按 Secret name 缓存整份 public values map：同 Secret 的不同 key 只触发一次 `secrets:read`，命中缓存不再 HTTP 或解密。缓存只存在于当前 provider 进程，新的 `tofu` / `mgcl run` 会重新读取；OpenTofu 在一次命令中可能启动多个 provider 进程，因此从 API 侧可能看到每个进程各一次请求。ephemeral resource 不使用该缓存，每次 Open 都重新请求。

> **不要在同一个 OpenTofu 项目里读写同一个 KV。** `mgcl_secret_kv` 的写入不会使 function 缓存失效；本 provider 不承诺写入与读取之间的幂等性、事务顺序或最终一致性。需要写入后再读取时，用独立项目分阶段 apply。

### managed resource

```hcl
resource "mgcl_secret_kv" "region" {
  key            = "app-config/region"
  value          = "us-east-1"
  ephemeral_mark = false # optional, default true
}
```

- `key`：required，`<secret-name>/<key>`；任一部分不符合 `^[a-z0-9][a-z0-9_-]{0,63}$` 时在 plan 阶段报错。修改 `key` 会替换资源（destroy 旧 KV，create 新 KV）。
- `value`：required，`Sensitive: true`，但仍完整进入 plan/state。
- `ephemeral_mark`：optional bool，默认 `true`。`true` 的 KV 不能被 `provider::mgcl::secret_value` 读取；`mgcl_secret` 会返回它。修改 value/mark 原地 update。
- Secret 不存在时由 provider API 原子创建；删除 KV 后 Secret 保留，可能为空。不实现 import。

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
go install github.com/geektr-cloud/terraform-provider-mgcl@v0.2.1
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
/opt/tofu-mirror/registry.opentofu.org/geektr-cloud/mgcl/0.2.1/linux_amd64/terraform-provider-mgcl_v0.2.1
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

也支持 packed 布局：`.../mgcl/terraform-provider-mgcl_0.2.1_linux_amd64.zip`。

## 行为与保证

- `provider "mgcl" {}` 无必填参数。`Configure` 只读环境变量，不访问网络，并把同一份 provider data 交给 ephemeral resource、managed resource 和 provider function。**不含 `provider::mgcl::secret_value` 的配置**在缺少 endpoint/token 时也能通过 `tofu validate`：validate 只检查 schema 与本地格式，不出网；第一次 Open 或 resource 操作才报缺少配置。**含 `provider::mgcl::secret_value` 的配置**在 validate 阶段就会求值 function，因此需要真实的 provider session（`mgcl run --` 注入的 endpoint/token），否则 validate 直接报未配置。OpenTofu 可能在 `Configure` 之前求值 provider function，此时函数直接从环境变量初始化，不发网络请求。
- `mgcl_secret` 的 `name` 为必填 string，语法 `^[a-z0-9][a-z0-9_-]{0,63}$`。`tofu validate` 在 name 已知时检查语法，unknown 时跳过；Open 对 null 返回 `Secret name is required`，对 unknown 返回 `Secret name must be known`。
- `provider::mgcl::secret_value` 的 `reference` 为 required string。null、unknown、非法格式在请求前报错。
- `mgcl_secret_kv` 的 `key` 为 required string，plan 阶段校验；`value` required；`ephemeral_mark` 默认 true。key 变更触发 replace，value/mark 变更原地 update。
- provider API 响应的 `name` 必须与请求一致；ephemeral resource、function 和 managed resource 都会丢弃名称不匹配的返回值并报错，不采用任何 value。name 不匹配诊断会 redact provider token。
- `mgcl_secret` 每次 Open 调用一次 `secrets:read`（access `ephemeral`），返回全部 KV 与 marks，不跨 Open 缓存。plan 与 apply 各自 Open，因此两次之间更新 Secret 后会读到新值。
- `provider::mgcl::secret_value` 只调用 `secrets:read` 且 access 为 `public`，返回 public KV。函数在每个 provider 进程内按 Secret name 缓存整份 public values map，带 mutex；同 Secret 不同 key、并发 miss 都只请求一次。只缓存成功、名称匹配且满足 per-KV mark 合同的响应：每个返回 key 必须有显式 `false` mark；缺失 mark、`true` mark 或混合非法响应整体报错，且不缓存、不返回任何 value。空响应（values 与 marks 都缺失或为空）是合法空 public 集，归一为非 nil 空 map；返回副本，调用方不能修改缓存。不同 Secret 分别请求。
- 函数缓存只存活当前 provider 进程，新的 `tofu` / `mgcl run` 重新读取。OpenTofu 在一次命令中可能启动多个 provider 进程，因此服务端可能看到每个进程各一次请求。`mgcl_secret_kv` 写入不会使函数缓存失效；不要在同一项目里读写同一 KV。
- `provider::mgcl::secret_value` 返回普通非 sensitive 数据，可被 plan/state/log 持久化。这是使用 public KV 的明确结果；KV 本身仍在 `mgcl.db` 中加密保存。
- `mgcl_secret_kv` 通过 `secret-kvs:write` 创建/覆盖 KV，通过 `secret-kvs:delete` 幂等删除 KV。Create/Update 不采用响应中的 value（响应本来也不含 value）；只校验响应里的 name、裸 key 和 mark。Read 用 `secrets:read` 同步 value/mark；Delete 不删除 Secret。
- `mgcl_secret_kv` 的 value 始终持久化在 plan/state；`Sensitive` 只控制展示。state 必须按敏感文件管理。
- 错误诊断不包含 provider token 或 Secret value；404、响应名称不匹配、public 读取的 per-KV mark 违约、key 缺失和 mark 缺失都有独立诊断，其他 HTTP 状态走通用读取失败诊断。
- 把 `mgcl_secret` 当普通 resource 使用（`resource "mgcl_secret" ...`）会直接报错；把 ephemeral 值用于会持久化的普通参数时，由 OpenTofu 核心拒绝。

## 排查

| 诊断 | 原因 |
|---|---|
| `MGCL_PROVIDER_ENDPOINT is not set` | 未在 `mgcl run -- <command>` 下运行，或该变量被覆盖 |
| `MGCL_PROVIDER_TOKEN is not set` | 同上；常驻 `mgcl run` 不签发 token |
| `Secret name is required` | `mgcl_secret.name` 为 null |
| `Secret name must be known` | name 依赖 plan 时未知的表达式；改为已知字面量或 plan 期间可知的值 |
| `Invalid secret name` | name 不符合 `^[a-z0-9][a-z0-9_-]{0,63}$` |
| `Invalid secret reference` | function 参数不是 `<secret-name>/<key>` 或 `<secret-name>/*`，或 name/key 非法 |
| `Invalid secret KV reference` | `mgcl_secret_kv.key` 不是合法的 `<secret-name>/<key>`，或使用了 wildcard |
| `Secret key "..." was not found among the public KVs` | KV 不存在，或它是 ephemeral mark；function 只读 public 集合 |
| `Failed to read public KVs` | `secrets:read`（access `public`）失败，例如 401/403/5xx；详情见诊断 |
| `mgcl secret "..." was not found; its public KV set could not be read` | function 请求的 Secret 不存在 |
| `Missing ephemeral mark in mgcl secret response` | Read 返回了 value 却没有对应 mark；provider 拒绝猜测 |
| `Unexpected mgcl secret response` | provider API 返回了不同 Secret 名称、public 读取违反 per-KV mark 合同、空响应或不匹配的写入元数据；返回值已被丢弃 |
| `Failed to read mgcl secret` | ephemeral Open 或 resource Read 遇到 401/404/服务端错误；详情见诊断 |
| `Failed to write mgcl secret KV` | `secret-kvs:write` 失败；详情见诊断 |
| `Failed to delete mgcl secret KV` | `secret-kvs:delete` 失败；详情见诊断 |

## 开发

```text
main.go                        providerserver 入口，address registry.opentofu.org/geektr-cloud/mgcl
internal/provider/provider.go  provider Metadata/Schema/Configure，共享 providerData 与 function 缓存
internal/provider/secret_ephemeral.go  mgcl_secret 的 Schema/ValidateConfig/Open/Close
internal/provider/secret_value_function.go  provider::mgcl::secret_value 的 Definition/Run
internal/provider/secret_kv_resource.go  mgcl_secret_kv 的 Schema/Create/Read/Update/Delete
internal/provider/secret_cache.go  public KV 的进程内缓存
e2e/                           真实 OpenTofu e2e（需要 PATH 中有 tofu >= 1.11）
```

校验：

```bash
gofmt -l .
go vet ./...
go test ./... -count=1
go test -race -short ./... -count=1
go build ./...
go mod verify
go mod tidy -diff
```

CI（`.github/workflows/ci.yml`）在 push `main`、pull request 和手动触发时运行，矩阵覆盖 OpenTofu `1.11.11` 与 `1.13.9`，执行 gofmt 检查、`go test ./... -count=1`、`go test -race -short ./... -count=1`、`go vet ./...`、`go build ./...`、`go mod verify`。

CI 和 Release 都只 checkout 本仓，在仓库根目录运行所有 Go 命令，GoReleaser 也从根目录构建。OpenTofu provider 依赖的 `github.com/geektr-cloud/openmgcl v0.2.1` 由 Go module proxy 或 module cache 提供，不需要同级 checkout。Release 由 `v*` tag 触发，checkout 该 tag 并从中构建。

发布由 `.github/workflows/release.yml` 驱动：推送 `v*` tag 后先运行完整 `go test ./... -count=1` 与 `go vet ./...`，再用 GoReleaser 构建 linux/darwin/windows × amd64/arm64 的 zip、`SHA256SUMS` 和 Registry manifest，创建 GitHub Release。GitHub Release 不会自动把 provider 加入 OpenTofu Registry。

`e2e/` 有两层测试，都会在缺少 `tofu >= 1.11` 或 `-short` 时明确 skip：

- **hermetic e2e**：用 `httptest` 假 API 验证 dev_overrides、plan/apply、空 Secret、function 单 KV 与 wildcard map、public KV 持久化、ephemeral KV 隔离、`mgcl_secret` 返回混合 KV、`mgcl_secret_kv` 创建/更新/替换/drift/destroy、非法 reference 零请求、误用与 filesystem mirror（unpacked 与 packed）init，不依赖 openmgcl 源码。假 API 实现 `secrets:read`、`secret-kvs:write`、`secret-kvs:delete`，按 KV mark 过滤 public 读取，并校验 token。binary plan 文件是 zip+Deflate，泄漏检查会逐 entry 解压后再扫描。
- **跨仓真实 e2e**：通过 `go list -m` 找到 `go.mod` 中解析的 openmgcl v0.2.1 源码（通常来自 Go module cache），把 `cmd/mgcl` 构建到临时目录，用临时 HOME、临时 0600 Ed25519 key 和临时 SQLite DB 执行 `mgcl secret create/set/list/show/get`，再经 `mgcl run --db ... --encryption-key-file ... -- tofu ...` 跑真实 OpenTofu。覆盖：不含 function 的配置在无 session 时 validate 通过、plan/apply 经真实 API 读取、更新 Secret 后新 run 读到新值、空 Secret、`--public` 后 function 读取单 KV 与 wildcard 并持久化 public 值、ephemeral KV 不可由 function 读、混合 Secret 的 `mgcl_secret` 返回全部 KV、`mgcl_secret_kv` 在同 Secret 创建/更新/替换/删除两个 KV、独立项目读取 resource 写入的 KV、ephemeral 值写入持久化参数被核心拒绝、provider token 跨 session 返回 401、非 loopback 安全门在加载 key/DB/listen 前失败。

所有端口由 listener 临时分配。**在 Go 依赖与工具链已缓存的前提下**，tofu/mgcl 运行期不访问外网；冷缓存时 `go build` 仍可能访问 Go module proxy。

跨仓测试只读取 Go module cache 中的 openmgcl v0.2.1 源码，构建产物写入临时目录；不要求同级 checkout，也不写入 module cache。

### 依赖说明

Provider 通过 `github.com/geektr-cloud/openmgcl/pkg/providerapi` 访问 mgcl provider API（stdlib-only 客户端）。

`require` 固定为 `github.com/geektr-cloud/openmgcl v0.2.1`，没有 `replace`。`ReadPublicSecret`、`WriteSecretKV`、`DeleteSecretKV`、per-KV `ephemeral_marks` 和 `secret set --public` 都包含在这个已发布模块中。本仓可脱离同级 openmgcl 目录独立构建，也支持 `go install github.com/geektr-cloud/terraform-provider-mgcl@v0.2.1`。

跨仓真实 e2e 通过 `go list -m` 解析到同一份 openmgcl v0.2.1 源码（通常位于 Go module cache），在临时目录构建 `mgcl` CLI，因此不依赖同级 checkout，也不写入 module cache。

Terraform Plugin Framework 版本见 `go.mod`（v1.19.0）。
