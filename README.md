# terraform-provider-mgcl

OpenTofu provider，把 [mgcl](https://github.com/geektr-cloud/openmgcl) 中的 bootstrap secret 以 **ephemeral resource** 形式提供给 OpenTofu 配置。

Provider 只提供一个 ephemeral resource：`mgcl_secret`。它不注册普通 resource、data source 或 provider function，因为这些类型的值会进入 plan 或 state。Secret value 只存在于 provider 进程内存中，由 OpenTofu 核心保证不会被写入 plan/state。

> 尚未发布到 OpenTofu Registry，也不是发布就绪状态。当前只能通过 `dev_overrides` 或 filesystem mirror 使用，详见下文。

## 要求

- **OpenTofu >= 1.11**（ephemeral resource 所需版本），provider source address 为 `registry.opentofu.org/geektr-cloud/mgcl`。
- Terraform 1.10+ 在语言层面支持 ephemeral resources，但本 provider 只在 OpenTofu >= 1.11 上测试，未发布到 Terraform Registry，也不以 Terraform 作为支持目标。
- **Go >= 1.27.4**（`go.mod` 的 `go` 指令）。
- 只支持在 `mgcl run -- <command>` 启动的子进程中运行。常驻 `mgcl run` 不签发 provider token。

> **开发期依赖（重要）**：`go.mod` 当前使用 `replace github.com/geektr-cloud/openmgcl => ../openmgcl`，这是主仓尚未发布阶段的**有意约束**。当前仓库不能脱离同级 `openmgcl` 目录独立构建，也不能用带 `@version` 的 `go install`（`replace` 不随模块分发，远程版本不存在）。正式发布前必须先给 openmgcl 打 tag、把 `require` 固定到真实发布版本并删除 `replace`。在此之前请勿把本仓当作可发布状态。

## 工作方式

Provider address：`registry.opentofu.org/geektr-cloud/mgcl`
二进制名：`terraform-provider-mgcl`

`provider "mgcl" {}` 没有任何 HCL 参数。连接信息只从环境变量读取，token 不允许写入配置文件或 HCL：

| 环境变量 | 含义 |
|---|---|
| `MGCL_PROVIDER_ENDPOINT` | provider API 的 base URL，例如 `http://127.0.0.1:1217`，由 `mgcl run --` 按实际 listener 注入 |
| `MGCL_PROVIDER_TOKEN` | 仅限本次子进程生命周期的只读 provider token，由 `mgcl run --` 注入 |

典型流程：

```bash
# 可选：dev.tfrc 放在 ~/.tofurc 时无需设置
export TF_CLI_CONFIG_FILE=/path/to/dev.tfrc
mgcl run --db ./mgcl.db -- tofu plan
```

`mgcl run --` 未设置 `TF_CLI_CONFIG_FILE` 时自动读取并合并默认的 `~/.tofurc`（存在时）；显式设置时使用指定文件，覆盖默认 `~/.tofurc`。合并结果写入临时 CLI config 传给子进程。

`mgcl run --` 只注入上面两个变量和既有的 `TF_*` 变量，不注入 Secret value。Secret value 由 provider 在 Open 时通过 `POST /api/mgcl/v1/secrets:read` 读取。

Provider token 是短期、只读的，只能读取当前 `mgcl.db` 中已知 Secret 的当前值。Secret 需先通过 `mgcl secret create` / `mgcl secret set` 离线准备好。

## HCL 示例

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

`values` 是 `map(string)` 且标记为 sensitive。需要多次引用时可用 locals 简化（ephemeral 属性会保持 ephemeral 语义）：

```hcl
locals {
  bootstrap = ephemeral.mgcl_secret.bootstrap.values
}

provider "aws" {
  region     = "us-east-1"
  access_key = local.bootstrap["access_key_id"]
  secret_key = local.bootstrap["secret_access_key"]
}
```

其他云 provider 的用法相同：从 `values["<key>"]` 取对应凭据。

## 本地安装

### 方式一：dev_overrides（快速开发）

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

`TF_CLI_CONFIG_FILE` 可选：未设置时 `mgcl run --` 自动继承 `~/.tofurc`；显式设置时以该变量为准，覆盖默认 `~/.tofurc`。`dev_overrides` 会跳过 `tofu init`；需要真正执行 `tofu init` 时改用方式二。

### 方式二：filesystem mirror（需要 tofu init）

需要走 `tofu init` 的集成场景使用符合 OpenTofu 布局的 filesystem mirror。最小 unpacked 布局：

```text
/opt/tofu-mirror/registry.opentofu.org/geektr-cloud/mgcl/0.1.0/linux_amd64/terraform-provider-mgcl_v0.1.0
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

也支持 packed 布局：`.../mgcl/terraform-provider-mgcl_0.1.0_linux_amd64.zip`。

## 行为与保证

- `provider "mgcl" {}` 无必填参数。`Configure` 只读环境变量，不访问网络；缺少 endpoint/token 时 `tofu validate` 仍然通过，由第一次 Open 给出明确诊断。
- `mgcl_secret` 的 `name` 为必填 string，语法 `^[a-z0-9][a-z0-9_-]{0,63}$`。`tofu validate` 在 name 已知时检查语法，unknown 时跳过；Open 对 null 返回 `Secret name is required`，对 unknown 返回 `Secret name must be known`，并要求 name 合法。
- provider API 响应的 `name` 必须与请求一致；不一致时丢弃返回值并报错，不采用任何 value。
- 每次 Open 调用一次 `secrets:read`，不跨 Open 缓存。plan 与 apply 各自 Open，因此两次之间更新 Secret 后会读到新值。
- 空 Secret 返回空 map（非 `null`）。
- Close 无持久化操作。
- 错误诊断不包含 token 或 Secret value。
- 把 `mgcl_secret` 当普通 resource 使用（`resource "mgcl_secret" ...`）会直接报错；把 ephemeral 值用于会持久化的普通参数时，由 OpenTofu 核心拒绝。

## 排查

| 诊断 | 原因 |
|---|---|
| `MGCL_PROVIDER_ENDPOINT is not set` | 未在 `mgcl run -- <command>` 下运行，或该变量被覆盖 |
| `MGCL_PROVIDER_TOKEN is not set` | 同上；常驻 `mgcl run` 不签发 token |
| `Secret name is required` | `name` 为 null；必须设置为非空字符串 |
| `Secret name must be known` | name 依赖 plan 时未知的表达式；改为已知字面量或 plan 期间可知的值 |
| `Invalid secret name` | name 不符合 `^[a-z0-9][a-z0-9_-]{0,63}$` |
| `Unexpected mgcl secret response` | provider API 返回了不同 Secret 名称或空响应；返回值已被丢弃 |
| `Failed to read mgcl secret` | 401 token 失效、404 Secret 不存在或服务端错误；详情见诊断 |

## 开发

```text
main.go                        providerserver 入口，address registry.opentofu.org/geektr-cloud/mgcl
internal/provider/provider.go  provider Metadata/Schema/Configure，只注册 ephemeral resource
internal/provider/secret_ephemeral.go  mgcl_secret 的 Schema/ValidateConfig/Open/Close
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
```

`e2e/` 有两层测试，都会在缺少 `tofu >= 1.11` 或 `-short` 时明确 skip：

- **hermetic e2e**：用 `httptest` 假 API 验证 dev_overrides、plan/apply、空 Secret、误用与 filesystem mirror（unpacked 与 packed）init，不依赖 openmgcl 源码。binary plan 文件是 zip+Deflate，泄漏检查会逐 entry 解压后再扫描。
- **跨仓真实 e2e**：通过 `go list -m` 找到 `go.mod` 中解析的 openmgcl 源码，现场构建 `cmd/mgcl`，用临时 HOME、临时 0600 Ed25519 key 和临时 SQLite DB 执行 `mgcl secret create/set/list/show/get`，再经 `mgcl run --db ... --encryption-key-file ... -- tofu ...` 跑真实 OpenTofu。覆盖：无 session 时 validate 通过、plan/apply 经真实 API 读取、更新 Secret 后新 run 读到新值、空 Secret、ephemeral 值写入持久化参数被核心拒绝、provider token 跨 session 返回 401、非 loopback 安全门在加载 key/DB/listen 前失败。

所有端口由 listener 临时分配。**在 Go 依赖与工具链已缓存的前提下**，tofu/mgcl 运行期不访问外网；冷缓存时 `go build` 仍可能访问 Go module proxy。

跨仓测试只读取 openmgcl 源码，不写入该仓库。

### 依赖说明

Provider 通过 `github.com/geektr-cloud/openmgcl/pkg/providerapi` 访问 mgcl provider API（stdlib-only 客户端）。

当前 `go.mod` 中的 `replace github.com/geektr-cloud/openmgcl => ../openmgcl` 是开发期设置（原因与发布前步骤见文首“开发期依赖”）。因此本仓现在**不能脱离同级 openmgcl 独立构建，也不能用于 `go install ...@version`**。正式发布前必须：给 openmgcl 打 tag、把 `require` 固定到该真实版本、删除 `replace`；未完成这些步骤前不要声称可发布。

Terraform Plugin Framework 版本见 `go.mod`（v1.19.0）。
