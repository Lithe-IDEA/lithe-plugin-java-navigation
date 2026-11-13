# codenav 的上游

本目录的 Java 声明索引、解析和使用处查找来自 [Viff](https://github.com/1lck/viff) 的
`internal/codenav`（MIT 许可，见仓库根目录 `THIRD_PARTY_NOTICES.md`）。

- 来源：Viff 工作副本 `viff-main`（2026-11-06），当时这部分代码还没有推到 `1lck/viff` 的任何分支，
  所以这里没有对应的上游提交。上游发布后，把这一行换成对应的提交 SHA。
- 设计取舍与实测数据：Viff 的 ADR 0040（跳转到声明与使用处）和 ADR 0041（跳转到 JDK）。

## 同步规则

- 除下面列出的文件和改动，本目录的 `.go` 文件与上游**逐字节一致**，不要在这里直接改；需要改解析规则时，
  先在 Viff 修（先补一条能失败的测试），再整目录拷回来。
- Lithe 新增的代码放在单独的文件里，文件开头注明“This file is a Lithe addition”：
  - `symbols.go`：文档结构（`textDocument/documentSymbol`）。
  - `overloads.go`：参数个数相同的重载按实参类型挑选。来自和 JDT LS 在 Nacos 上的对比：唯一跳错的
    两处都是同参数个数的重载（`new AccessException("...")` 跳到了 `AccessException(int)`）。
  - `qualified.go`：显式类型实参调用 `Foo.<T>bar()` 和限定名注解 `@Outer.Inner(...)`。
  - `jdtls_cases_test.go`：上面几条以及可变参数参数的测试，都来自同一次对比。
- Viff 还没有发布 codenav，这些只能先在这里改。它们对上游文件的改动只有下面几处，
  上游发布后把这些改动连同上面的新文件提给 Viff，再恢复逐字节一致：
  - `parse.go`：`Member` 加 `Params` 字段，解析成员时赋值 `m.Params = p.paramTypes(j, pe)`。
  - `resolve.go`：`findMethod`、`pickCtor`、`methodInScope` 的参数个数换成 `call`（参数个数加左括号位置），
    参数个数相同的候选交给 `bestOverload`；`resolveAt` 和 `typeOfExpr` 的限定符用 `qualifierDot` 找，
    跳过显式类型实参；限定名注解在 `resolveAt` 里按嵌套类型解析（`annotationName`）；`findLocal` 认出
    `String... keys` 这样的可变参数参数（原来类型在 `...` 前面时找不到）。
  - 依赖库跳转（见 `internal/deps`）：
    - `library.go`：`Library` 也装依赖 jar 的源码或签名存根，库路径可以是 `<jar 编号>!a/b/C.java`
      （`LibraryFQN` 认这种形式）；同一个类出现在多个路径时取第一个（JDK 在前，依赖按 classpath 顺序）；
      `paths` 的键改成类名的斜杠形式，并且是路径的子串，去掉了 `srcs` 表，15 万个类时少占约 10 MiB。
    - `doc.go`：说明里加上依赖 jar。
  - 内存：`lex.go` 加 `lexInto`，`parse.go` 加复用分词缓冲的 `FileParser` 和 `parseTokens`；
    建索引分配的内存从 198 MB 降到 86 MB（Nacos）。
  - 类型推断：`resolve.go` 的 `typeOfExpr` 认出字符串字面量和文本块（`"a".trim()`）、类字面量
    （`Foo.class.getName()`）和 `Iface.super.m()`；`parse.go` 记下 Lombok 日志注解生成的 logger 类型
    （`Type.LogType`，`@Slf4j` 是 `org.slf4j.Logger`），`findField` 给 `log` 字段带上这个类型，
    `log.info(...)` 才能跳进日志库。
  - 局部变量：`resolve.go` 的 `findLocal` 认出一条声明里逗号后面的变量（`int i = 0, size = s.length();`、
    `int a, b;`），沿用第一个变量的类型；初始化表达式里括号更深的名字不算。
- 同步后运行 `go test ./...`。

`internal/jdk/find.go` 改编自 Viff 的 `internal/platform/jdk.go`（去掉了 Viff 的进程辅助函数，
只在 macOS 上起的 `java_home` 进程本来就没有控制台窗口）；`internal/jdk/sources.go` 改编自 Viff 的
`internal/app/jdk.go`，加了把单个源码文件解压成只读副本的 `Extract`。
