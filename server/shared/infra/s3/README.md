# shared/infra/s3 —— S3 兼容对象存储

**文件字节都在这里，PostgreSQL 只存元信息。** 附件（issue / 建议）、技能文件、技能压缩包、
个人文件（`/api/files`）的字节写进对象存储；PG 的 `files` / `user_files` 只记桶、key、大小、哈希。
PG 档下没配 `s3.bucket`，amad 拒绝启动（`cmd/amad` 的 `openBlobStore`），不会退回去把字节写进库。

客户端是通用 S3（`aws-sdk-go-v2/service/s3`，版本钉在 `server/go.mod`，兼容 `go 1.24`），
不绑任何一家。本地 docker compose 起的是 RustFS（`rustfs/rustfs:1.0.4`），生产可以换成
AWS S3 / MinIO / Ceph RGW / 云厂商的 S3 兼容服务，只改 `s3` 段配置。

## 谁用它

| 调用方 | 适配层（`cmd/amad/s3store.go`） | 用到的方法 |
| --- | --- | --- |
| 附件、技能文件、技能包（`core.BlobStore`） | `blobStore` | Put / Get / Delete / Size / 预签名（按读者选地址） |
| 个人文件（`userfile.ObjectStore`） | `userFileObjects` | Put / Delete / Size / Open / 预签名（浏览器）/ 分片四件套 |

本包只导出 `ErrNotFound`。适配层把它翻译成 `core.ErrBlobNotFound` /
`userfile.ErrObjectNotFound`——业务包不 import 本包，**漏了翻译 `errors.Is` 永远为假**
（技能包缓存认不出 miss、个人文件分不清「对象没了」），零报错。`cmd/amad/s3store_test.go`
对真对象存储钉着这件事。

## ⚠️ 三个 endpoint：预签名链接把 Host 签进去了

SigV4 预签名把 **Host** 一起签进去，同一条链接只能用签名时那个地址打，换个 Host 就是
403 SignatureDoesNotMatch。而三方能访问到的地址不一样：

| 配置 | 谁用 | compose 默认 |
| --- | --- | --- |
| `s3.endpoint` | amad 自己读写 | `http://ama-s3:9000`（ama-net 上的别名） |
| `s3.publicEndpoint` | **浏览器**（个人文件下载）、**用户自带机器**（`ama daemon` 拉技能包） | `http://localhost:19000` |
| `s3.sandboxEndpoint` | **Docker 会话容器**（拉技能包） | 留空 = 同 endpoint |

后两个留空回落 `endpoint`。签名是纯本地计算，每个地址各建一个签名用的 client，不发请求。

签错的症状**都不指向配置**：

- 浏览器点个人文件下载 403 / 打不开 → `publicEndpoint` 不是浏览器能访问的地址
  （浏览器不在本机时要改成 `http://<本机 IP 或域名>:19000`）；
- 会话容器里技能只有 SKILL.md、附件全缺 → 容器拉技能包失败后静默回落，日志里一条
  「技能包下载失败」Warn；查 `sandboxEndpoint` 与 `docker.network`（容器要接在 ama-net 上）。

⚠️ 别想着「签完再改 Host」（签名验不过），也别指望一个地址两边都通：compose 的服务名
浏览器解析不了，`localhost` 在容器里指向容器自己。

**不走预签名的几条路**（与对象存储地址无关）：

- issue / 建议附件下载经 amad 中转（`GET /api/issue-attachments/{id}` 等），判权在 amad 里；
- `ama files download` 打 `GET /api/files/{id}/content`，由 amad 流式转发——它常跑在会话容器里，
  那里打不通给浏览器签的链接。`ama files url` 仍然打印预签名链接给浏览器用。

## 其余几条

- **path-style 默认开**（`s3.usePathStyle` 留空为 true）：RustFS / MinIO / 本地都要；
  虚拟主机式需要通配 DNS。用 AWS S3 想走虚拟主机式时显式写 false。
- **启动时确认桶可用**：`HeadBucket`；不存在且 `s3.createBucket`（留空 true）就建。
  多实例同时建会撞 `BucketAlreadyOwnedByYou` / `BucketAlreadyExists`，都当成功。
  凭据错、地址错都在启动时炸，而不是等第一次上传才 500。
- **请求校验和只在协议要求时带**（`RequestChecksumCalculation: WhenRequired`）：新版 SDK 默认
  给每个请求加 CRC32 尾随校验和（aws-chunked），不少 S3 兼容服务不认。
- **分片先读进内存再发**：http（本地 / 内网常态）下 SigV4 要算请求体 SHA-256，SDK 要求可 Seek，
  而 `http.Request.Body` 不行；读进来还能当场核对「真实字节数 == 声明的 size」。
  片大小由 `service/userfile` 限定为 8 MiB，内存有界。
- `Open` 不套单次超时（个人文件可到 2 GiB），生命周期跟调用方的 ctx 走。

## 测试

离线部分（签名 Host、配置校验、建桶容错）直接跑。集成部分对真对象存储跑，没配就 Skip：

```bash
docker compose up -d s3
AMA_TEST_S3_ENDPOINT=http://127.0.0.1:19000 \
AMA_TEST_S3_ACCESS_KEY=ama-dev-access AMA_TEST_S3_SECRET_KEY=ama-dev-secret-change-me \
  go test ./shared/infra/s3/ ./cmd/amad/
```

每次用一个随机新桶（`ama-test-xxxxxxxx`），顺带测「桶不存在就建」。
