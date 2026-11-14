# Linux 构建与验证

`node scripts/package.mjs` 使用 `CGO_ENABLED=0` 交叉编译 Linux ARM64 与 x64 语言服务器，分别产生 `-linux-arm64.zip`、`-linux-x64.zip`。归档检查校验 ELF64 架构、包内启动路径与解压后的执行权限。

`node scripts/package/smoke-linux.mjs <zip>` 在对应原生 Linux runner 上启动包内程序，完成 LSP initialize、文档符号与 shutdown，验证安装目录文件内容与权限不变，并清理进程组和临时工作区。

## 资源与验证边界

所有下载、解压、编译与归档都属于本插件仓库的构建阶段。`dist/` 为输出包；`.artifacts/`、`.cache/`、`target/` 或工具自身缓存为本地构建状态，不跨工作树共享可变状态。校验过的下载以固定来源与摘要识别，提取结果不作为可信下载源。运行时只读取已安装插件，缓存、日志、索引和工作区状态归用户可写目录。Linux 不使用 macOS 代码签名或 Sparkle；本次不改变 macOS 包签名与更新基线。

原生 ARM64 和 x64 验证由 `.github/workflows/ci.yml` 执行；本机的交叉编译与归档检查不能替代原生运行结果。只有目标平台 CI 成功后才能宣称该平台通过。发布新版本后，使用实际 Release 附件的 SHA-256 更新 Store，保留所有旧版本与既有平台。
