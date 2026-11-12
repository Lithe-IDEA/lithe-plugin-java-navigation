# codenav 的上游

本目录的 Java 声明索引、解析和使用处查找来自 [Viff](https://github.com/1lck/viff) 的
`internal/codenav`（MIT 许可，见仓库根目录 `THIRD_PARTY_NOTICES.md`）。

- 来源：Viff 工作副本 `viff-main`（2026-11-06），当时这部分代码还没有推到 `1lck/viff` 的任何分支，
  所以这里没有对应的上游提交。上游发布后，把这一行换成对应的提交 SHA。
- 设计取舍与实测数据：Viff 的 ADR 0040（跳转到声明与使用处）和 ADR 0041（跳转到 JDK）。

## 同步规则

- 除下面列出的文件，本目录的 `.go` 文件与上游**逐字节一致**，不要在这里直接改；需要改解析规则时，
  先在 Viff 修（先补一条能失败的测试），再整目录拷回来。
- Lithe 新增的代码放在单独的文件里，文件开头注明“This file is a Lithe addition”：
  - `symbols.go`：文档结构（`textDocument/documentSymbol`）。
- 同步后运行 `go test ./...`。

`internal/jdk/find.go` 改编自 Viff 的 `internal/platform/jdk.go`（去掉了 Viff 的进程辅助函数，
只在 macOS 上起的 `java_home` 进程本来就没有控制台窗口）；`internal/jdk/sources.go` 改编自 Viff 的
`internal/app/jdk.go`，加了把单个源码文件解压成只读副本的 `Extract`。
